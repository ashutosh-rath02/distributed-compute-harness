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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/statebundle"
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
	keepAwake := flag.Bool("keep-awake", true, "ask the OS not to sleep while your devices are working on tasks (and 2 minutes after): a sleeping manager stops the whole fleet. It never blocks closing the lid or choosing Sleep")
	aiKeyFile := flag.String("ai-key-file", "", "file holding the AI key: the API key apps use for the OpenAI-compatible API (/v1), which opens nothing else. Created (owner-only) on first run; default: \"ai-key\" next to -operator-token-file. Delete it and restart to revoke it")
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
	allowRaw := flag.Bool("allow-raw-commands", false, "on first start (no policy stored yet), allow raw commands (system.execute) and host file reads (filesystem.read) on every device; otherwise only the sandboxed typed task types run until enabled with \"harnessctl policy\"")
	artifactDir := flag.String("artifact-dir", "harness-artifacts", "directory holding workload input/output files (content-addressed); \"\" disables workload files")
	artifactMax := flag.String("artifact-max-size", "256MiB", "largest single workload file accepted (upload or output)")
	artifactTotal := flag.String("artifact-store-size", "4GiB", "most space all stored workload files may use; uploads beyond it are refused")
	artifactRetention := flag.Duration("artifact-retention", 7*24*time.Hour, "how long a stored file no queued/running/restarting workload needs is kept after it was last used")
	advertiseAddr := flag.String("advertise-addr", "", "this machine's LAN address agents should use (host:port), shown pre-filled in the dashboard's \"add a device\" form — e.g. the Android app passes the phone's Wi-Fi address")
	joinAddr := flag.String("join-addr", "", "serve the join page here (plain HTTP, e.g. :7419): devices open http://<this machine>:7419 to install an agent that joins by approval on the dashboard. Off when empty")
	joinWindow := flag.Duration("join-window", 15*time.Minute, "how long adding devices stays open when the manager starts with no devices yet (0 = closed until opened from the dashboard); later it is opened from the dashboard's \"Add a device\"")
	appAPK := flag.String("app-apk", "", "the Android app's APK, offered for download on the join page (the app passes its own)")
	exportState := flag.String("export-state", "", "write the -state-dir (tokens, database, TLS certificate) to this zip and exit; the manager must be stopped. The file holds the TLS key and tokens: keep it private and delete it after importing")
	importState := flag.String("import-state", "", "unpack a state zip made by -export-state into -state-dir and exit (refuses to replace an existing database without -force)")
	stateDir := flag.String("state-dir", "", "the state directory for -export-state / -import-state")
	withArtifacts := flag.Bool("with-artifacts", false, "with -export-state: include stored workload files (can be large)")
	force := flag.Bool("force", false, "with -import-state: replace an existing state")
	flag.Parse()

	if *exportState != "" || *importState != "" {
		if *stateDir == "" {
			log.Fatal("manager: -state-dir is required with -export-state / -import-state")
		}
		if err := moveState(*exportState, *importState, *stateDir, *withArtifacts, *force); err != nil {
			log.Fatalf("manager: %v", err)
		}
		return
	}

	if *advertiseAddr == "" {
		*advertiseAddr = detectLANAddr(*addr)
	}

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
	if *aiKeyFile == "" {
		*aiKeyFile = filepath.Join(filepath.Dir(*operatorTokenFile), "ai-key")
	}
	aiKey, err := manager.LoadOrCreateOperatorToken(*aiKeyFile)
	if err != nil {
		log.Fatalf("manager: AI key: %v", err)
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
		AIKey:               aiKey,
		KeepAwake:           *keepAwake,
		Artifacts:           artifactStore,
		ArtifactRetention:   *artifactRetention,
		InitialPolicy:       initialPolicy(*allowRaw),
		AdvertiseAddr:       *advertiseAddr,
		JoinAddr:            *joinAddr,
		FirstRunJoinWindow:  *joinWindow,
		AppAPK:              *appAPK,
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

	if *joinAddr != "" {
		joinServer := &http.Server{Addr: *joinAddr, Handler: srv.JoinPageHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := joinServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("manager: join page stopped: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			joinServer.Close()
		}()
		where := "<this machine's LAN address>"
		if host, _, err := net.SplitHostPort(*advertiseAddr); err == nil && host != "" {
			where = host
		}
		log.Printf("manager: to add a device, open http://%s%s on it (while adding devices is open)", where, portSuffix(*joinAddr))
	}

	apiServer := &http.Server{Addr: *apiAddr, Handler: srv.NewHTTPHandler()}
	// Keeps trying while the API port is taken instead of running on
	// without an API until someone restarts the manager: on a phone,
	// another app can hold 127.0.0.1:7421 for a while (the Android app
	// checks /server-proof before signing in, so that app learns nothing).
	go func() {
		failing := false
		for {
			ln, err := net.Listen("tcp", *apiAddr)
			if err == nil {
				if failing {
					log.Printf("manager: API listening on %s again", *apiAddr)
				}
				failing = false
				if err = apiServer.Serve(ln); err == http.ErrServerClosed {
					return
				}
			}
			if !failing {
				log.Printf("manager: API server stopped: %v (retrying every 5s)", err)
				failing = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
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

// initialPolicy is the policy a manager starts with when none is stored.
func initialPolicy(allowRaw bool) *domain.Policy {
	p := domain.DefaultPolicy()
	if allowRaw {
		for _, name := range []domain.CapabilityName{domain.CapabilitySystemExecute, domain.CapabilityFilesystemRead} {
			p.Types[name] = domain.TypePolicy{Enabled: true}
		}
	}
	return &p
}

// moveState runs -export-state or -import-state.
func moveState(exportTo, importFrom, stateDir string, withArtifacts, force bool) error {
	if exportTo != "" {
		f, err := os.OpenFile(exportTo, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err := statebundle.Export(stateDir, f, withArtifacts); err != nil {
			f.Close()
			os.Remove(exportTo)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Printf("State exported to %s. It holds the TLS key and tokens: move it to the new install, import it, then delete it.\n", exportTo)
		return nil
	}
	f, err := os.Open(importFrom)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	if err := statebundle.Import(f, info.Size(), stateDir, force); err != nil {
		return err
	}
	fmt.Printf("State imported into %s.\n", stateDir)
	return nil
}

// portSuffix is ":port" of a listen address (":7419" from ":7419" or
// "0.0.0.0:7419").
func portSuffix(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return addr
}

// detectLANAddr guesses this machine's LAN address for agents (host:port
// with -addr's port): the local address of the route to the internet
// (a UDP "dial" sends nothing). Only a private address, and only when
// -addr listens on every interface; otherwise none (the dashboard then
// asks). The Android app passes its own, since an app can't do this.
func detectLANAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" || (host != "" && host != "0.0.0.0" && host != "::") {
		return ""
	}
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1: never reached
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if !ip.IsPrivate() {
		return ""
	}
	return net.JoinHostPort(ip.String(), port)
}
