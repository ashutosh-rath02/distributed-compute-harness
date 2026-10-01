// Command manager runs the harness control plane: it accepts agent
// connections, tracks the node registry, and (unless disabled) announces
// itself on the local network so agents don't need a hardcoded address.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/multi"
	"home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

func main() {
	addr := flag.String("addr", ":7420", "address to listen on for agent connections (host:port)")
	apiAddr := flag.String("api-addr", "127.0.0.1:7421", "address to serve the HTTP observability/control API on (loopback by default: POST /nodes/{id}/commands has no auth of its own, so widening this exposes unauthenticated command dispatch to the network)")
	dbPath := flag.String("db", "harness-manager.db", "path to the persistent store file")
	tlsDir := flag.String("tls-dir", "harness-manager-tls", "directory holding the manager's persistent TLS certificate")
	operatorTokenFile := flag.String("operator-token-file", "harness-operator-token", "file holding the operator API token, created (owner-only) on first run. Every API call needs it: harnessctl reads this file, and the dashboard signs in through the login link logged at startup. Delete it and restart to revoke every client")
	pairingToken := flag.String("pairing-token", "", "shared secret agents must present to register (required)")
	heartbeatTimeout := flag.Duration("heartbeat-timeout", 15*time.Second, "how long without a heartbeat before a node is marked offline")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "how often to check for workloads that need restarting (RestartPolicy on-failure/always)")
	var agentBinaries agentBinaryFlags
	checkAgentBinaries := flag.Bool("check-agent-binaries", false, "only validate the -agent-binary set (platform detection, conflicts, duplicates), print it, and exit 0 if usable or 1 if not — lets an installer vet a new set before stopping a running manager")
	flag.Var(&agentBinaries, "agent-binary", "agent executable to serve for onboarding and self-update; repeat once per platform (e.g. a Windows agent.exe and a linux/arm64 build for Android). Each value is a path, whose platform is read from the file, or os/arch=path to state it explicitly. Onboarding downloads and self-update are disabled if unset")
	disableDiscovery := flag.Bool("disable-discovery", false, "disable the LAN multicast discovery beacon")
	insecure := flag.Bool("insecure", false, "disable TLS: agents connect over plaintext ws:// with no manager authentication (dev/local use only). POST /workloads still returns 202 and dispatches ASSIGN, but an agent run with its own -insecure will refuse to execute it (see cmd/agent's -insecure) rather than run arbitrary code for a manager it can't verify")
	relayAddr := flag.String("relay-addr", "", "address of a relay server (cmd/relay) to also accept connections through, for agents that aren't on this manager's LAN; disabled if unset")
	relayToken := flag.String("relay-token", "", "shared secret identifying this manager's session at the relay (required if -relay-addr is set; same care as -pairing-token: long, random, not reused)")
	relayPublicURL := flag.String("relay-public-url", "", "browser-trusted HTTPS base URL exposed by the relay for internet enrollment links (e.g. https://relay.example.com:8443)")
	relayEnrollmentToken := flag.String("relay-enrollment-token", "", "secret authorizing this manager to publish internet enrollment links (required with -relay-public-url)")
	artifactDir := flag.String("artifact-dir", "harness-artifacts", "directory holding workload input/output files (content-addressed); \"\" disables workload files")
	artifactMax := flag.String("artifact-max-size", "256MiB", "largest single workload file accepted (upload or output)")
	artifactTotal := flag.String("artifact-store-size", "4GiB", "most space all stored workload files may use; uploads beyond it are refused")
	artifactRetention := flag.Duration("artifact-retention", 7*24*time.Hour, "how long a stored file no queued/running/restarting workload needs is kept after it was last used")
	flag.Parse()

	if *checkAgentBinaries {
		catalog, err := manager.BuildAgentCatalog(agentBinaries)
		if err != nil {
			log.Fatalf("manager: -agent-binary: %v", err)
		}
		for _, line := range catalog.Describe() {
			fmt.Println(line)
		}
		return
	}

	if *pairingToken == "" {
		log.Fatal("manager: -pairing-token is required")
	}
	if *relayAddr != "" && *relayToken == "" {
		log.Fatal("manager: -relay-token is required when -relay-addr is set")
	}
	if *relayPublicURL != "" && *relayAddr == "" {
		log.Fatal("manager: -relay-addr is required when -relay-public-url is set")
	}
	if *relayPublicURL != "" && *relayEnrollmentToken == "" {
		log.Fatal("manager: -relay-enrollment-token is required when -relay-public-url is set")
	}
	if *relayPublicURL != "" {
		u, err := url.Parse(*relayPublicURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			log.Fatal("manager: -relay-public-url must be a browser-trusted HTTPS origin, e.g. https://relay.example.com:8443")
		}
	}

	// Validated up front, before any disk I/O, like the flags above: a
	// manager that silently came up without the builds an operator asked
	// for would fail much later, at the first invitation or update.
	if _, err := manager.BuildAgentCatalog(agentBinaries); err != nil {
		log.Fatalf("manager: -agent-binary: %v", err)
	}

	operatorToken, err := manager.LoadOrCreateOperatorToken(*operatorTokenFile)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	store, err := persistent.Open(*dbPath)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}
	defer store.Close()

	var cert tls.Certificate
	transport := ws.New()
	var fingerprint string
	if !*insecure {
		var err error
		cert, err = mtls.LoadOrCreateCert(*tlsDir)
		if err != nil {
			log.Fatalf("manager: %v", err)
		}
		fingerprint = mtls.Fingerprint(cert)
		log.Printf("Manager TLS fingerprint (give this to agents via -manager-fingerprint):\n  %s", fingerprint)
		transport = ws.NewTLSServer(cert)
	} else {
		log.Println("manager: running with -insecure: plaintext transport, no manager authentication")
	}

	// finalTransport is what the manager actually listens with — just the
	// LAN transport, unless -relay-addr opts into also accepting
	// connections relayed from off-LAN agents (v5 remote part 1). Composed
	// here at the composition root (multi.Transport), not inside
	// manager.Server, so neither transport package nor Server needs to
	// know the other listening path exists.
	var finalTransport domain.Transport = transport
	var relayTransport *relay.Transport
	if *relayAddr != "" {
		if !*insecure {
			relayTransport = relay.NewTLSServer(*relayToken, cert)
		} else {
			relayTransport = relay.New(*relayToken)
		}
		finalTransport = multi.New().Add(transport, *addr).Add(relayTransport, *relayAddr)
		log.Printf("manager: also accepting connections via relay at %s", *relayAddr)
	}

	var enrollmentPublisher manager.EnrollmentPublisher
	if *relayPublicURL != "" {
		enrollmentPublisher = &manager.HTTPEnrollmentPublisher{URL: *relayPublicURL, RelaySession: *relayToken, PublishToken: *relayEnrollmentToken}
	}
	var artifactStore *artifacts.Store
	if *artifactDir != "" {
		maxBytes, err := parseSize(*artifactMax)
		if err != nil {
			log.Fatalf("manager: -artifact-max-size: %v", err)
		}
		totalBytes, err := parseSize(*artifactTotal)
		if err != nil {
			log.Fatalf("manager: -artifact-store-size: %v", err)
		}
		artifactStore, err = artifacts.Open(artifacts.Config{Dir: *artifactDir, MaxBytes: maxBytes, TotalBytes: totalBytes})
		if err != nil {
			// Like a bad -agent-binary set: lose the feature, keep the
			// manager (a launcher would otherwise crash-loop at every boot).
			log.Printf("manager: WORKLOAD FILES DISABLED: %v", err)
			artifactStore = nil
		} else {
			used, total := artifactStore.Usage()
			log.Printf("manager: workload file store at %s (%d of %d bytes used)", *artifactDir, used, total)
		}
	}
	srv := manager.NewServer(finalTransport, store, manager.Config{
		Addr:                *addr,
		PairingToken:        *pairingToken,
		HeartbeatTimeout:    *heartbeatTimeout,
		ReconcileInterval:   *reconcileInterval,
		AgentBinaries:       agentBinaries,
		Fingerprint:         fingerprint,
		RelayAddr:           *relayAddr,
		RelayToken:          *relayToken,
		RelayPublicURL:      *relayPublicURL,
		EnrollmentPublisher: enrollmentPublisher,
		OperatorToken:       operatorToken,
		Artifacts:           artifactStore,
		ArtifactRetention:   *artifactRetention,
	})
	// Registered before Run (which calls transport.Listen) — puts the
	// download on the exact address/port agents already dial, no new port
	// or firewall rule needed for self-update (internal/agent/selfupdate.go).
	// /agent-binary is the legacy single route agents predating the
	// catalog still download from; /agent-binaries/{os}/{arch} serves each
	// platform's build.
	transport.Handle("/agent-binary", srv.AgentBinaryHandler())
	transport.Handle("GET /agent-binaries/{os}/{arch}", srv.AgentBinariesHandler())
	transport.Handle("/enroll/", srv.EnrollmentHandler())
	// Workload file transfers, authorized per assignment (manager/artifacts.go).
	transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	if relayTransport != nil {
		relayTransport.Handle("/agent-binary", srv.AgentBinaryHandler())
		relayTransport.Handle("GET /agent-binaries/{os}/{arch}", srv.AgentBinariesHandler())
		relayTransport.Handle("/enroll/", srv.EnrollmentHandler())
		relayTransport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !*disableDiscovery {
		port, err := portOf(*addr)
		if err != nil {
			log.Fatalf("manager: -addr must include a port for discovery (or pass -disable-discovery): %v", err)
		}
		beacon := &udp.Beacon{ManagerPort: port}
		go func() {
			if err := beacon.Run(ctx); err != nil {
				log.Printf("manager: discovery beacon stopped: %v", err)
			}
		}()
	}

	apiServer := &http.Server{Addr: *apiAddr, Handler: srv.NewHTTPHandler()}
	go func() {
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("manager: API server stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		apiServer.Shutdown(shutdownCtx)
	}()

	log.Printf("harness manager listening on %s (API on %s)", *addr, *apiAddr)
	log.Printf("manager: dashboard sign-in link (keep it private, it grants full control): %s", manager.LoginURL(*apiAddr, operatorToken))
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager: %v", err)
	}
}

// agentBinaryFlags collects repeated -agent-binary values.
type agentBinaryFlags []manager.AgentBinary

func (f *agentBinaryFlags) String() string {
	parts := make([]string, 0, len(*f))
	for _, b := range *f {
		parts = append(parts, b.Path)
	}
	return strings.Join(parts, ",")
}

func (f *agentBinaryFlags) Set(value string) error {
	b, err := manager.ParseAgentBinaryFlag(value)
	if err != nil {
		return err
	}
	*f = append(*f, b)
	return nil
}

func portOf(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return port, nil
}

// parseSize parses a positive human-readable byte size flag ("256MiB").
func parseSize(v string) (int64, error) {
	n, err := domain.ParseByteSize(v)
	if err != nil {
		return 0, err
	}
	if n == 0 || n > 1<<50 {
		return 0, fmt.Errorf("size %q out of range", v)
	}
	return int64(n), nil
}
