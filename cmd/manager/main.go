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
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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
	pairingToken := flag.String("pairing-token", "", "shared secret agents must present to register (required)")
	heartbeatTimeout := flag.Duration("heartbeat-timeout", 15*time.Second, "how long without a heartbeat before a node is marked offline")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "how often to check for workloads that need restarting (RestartPolicy on-failure/always)")
	agentBinaryPath := flag.String("agent-binary", "", "path to the agent executable to serve for self-update (POST /nodes/{id}/update); self-update disabled if unset")
	disableDiscovery := flag.Bool("disable-discovery", false, "disable the LAN multicast discovery beacon")
	insecure := flag.Bool("insecure", false, "disable TLS: agents connect over plaintext ws:// with no manager authentication (dev/local use only). POST /workloads still returns 202 and dispatches ASSIGN, but an agent run with its own -insecure will refuse to execute it (see cmd/agent's -insecure) rather than run arbitrary code for a manager it can't verify")
	relayAddr := flag.String("relay-addr", "", "address of a relay server (cmd/relay) to also accept connections through, for agents that aren't on this manager's LAN; disabled if unset")
	relayToken := flag.String("relay-token", "", "shared secret identifying this manager's session at the relay (required if -relay-addr is set; same care as -pairing-token: long, random, not reused)")
	flag.Parse()

	if *pairingToken == "" {
		log.Fatal("manager: -pairing-token is required")
	}
	if *relayAddr != "" && *relayToken == "" {
		log.Fatal("manager: -relay-token is required when -relay-addr is set")
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
	if *relayAddr != "" {
		var relayTransport domain.Transport
		if !*insecure {
			relayTransport = relay.NewTLSServer(*relayToken, cert)
		} else {
			relayTransport = relay.New(*relayToken)
		}
		finalTransport = multi.New().Add(transport, *addr).Add(relayTransport, *relayAddr)
		log.Printf("manager: also accepting connections via relay at %s", *relayAddr)
	}

	srv := manager.NewServer(finalTransport, store, manager.Config{
		Addr:              *addr,
		PairingToken:      *pairingToken,
		HeartbeatTimeout:  *heartbeatTimeout,
		ReconcileInterval: *reconcileInterval,
		AgentBinaryPath:   *agentBinaryPath,
		Fingerprint:       fingerprint,
	})
	// Registered before Run (which calls transport.Listen) — puts the
	// download on the exact address/port agents already dial, no new port
	// or firewall rule needed for self-update (internal/agent/selfupdate.go).
	transport.Handle("/agent-binary", srv.AgentBinaryHandler())

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
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager: %v", err)
	}
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
