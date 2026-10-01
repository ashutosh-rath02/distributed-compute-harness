// Command agent runs a harness node: it discovers (or is pointed at) the
// manager, registers with a persistent identity, and heartbeats until
// stopped, reconnecting automatically on any failure.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/domain"
	"home-harness/internal/instancelock"
	"home-harness/internal/mtls"
	"home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

func main() {
	managerAddr := flag.String("manager-addr", "", "manager address host:port; if empty, discover it via LAN multicast")
	pairingToken := flag.String("pairing-token", "", "shared secret required to register (required)")
	identityDir := flag.String("identity-dir", defaultIdentityDir(), "directory holding this node's persistent identity")
	name := flag.String("name", "", "friendly node name (defaults to hostname)")
	heartbeatInterval := flag.Duration("heartbeat-interval", 5*time.Second, "how often to send heartbeats")
	managerFingerprint := flag.String("manager-fingerprint", "", "expected SHA-256 fingerprint of the manager's TLS certificate, printed on manager startup (required unless -insecure)")
	insecure := flag.Bool("insecure", false, "disable TLS: connect over plaintext ws:// with no manager authentication (dev/local use only; must match the manager's -insecure)")
	relayAddr := flag.String("relay-addr", "", "relay server (cmd/relay) address to connect through, for a manager that isn't on this device's LAN; if set, -manager-addr/discovery are not used")
	slots := flag.Int("slots", 0, "how many workloads this agent runs at once (0 = half the CPU count, at most 16)")
	relayToken := flag.String("relay-token", "", "the manager's relay session token (required if -relay-addr is set)")
	flag.Parse()

	if *pairingToken == "" {
		log.Fatal("agent: -pairing-token is required")
	}
	if *relayAddr != "" && *relayToken == "" {
		log.Fatal("agent: -relay-token is required when -relay-addr is set")
	}

	var transport domain.Transport
	var selfUpdateHTTPClient *http.Client
	var selfUpdateURL string
	switch {
	case *relayAddr != "" && !*insecure:
		if *managerFingerprint == "" {
			log.Fatal("agent: -manager-fingerprint is required unless -insecure is set (get it from the manager's startup log)")
		}
		relayTransport := relay.NewTLSClient(*relayToken, mtls.PinnedClientConfig(*managerFingerprint))
		transport = relayTransport
		selfUpdateHTTPClient = relayTransport.HTTPClient(*relayAddr)
		selfUpdateURL = "http://manager"
	case *relayAddr != "":
		log.Println("agent: running with -insecure: plaintext transport, manager identity not verified")
		relayTransport := relay.NewClient(*relayToken)
		transport = relayTransport
		selfUpdateHTTPClient = relayTransport.HTTPClient(*relayAddr)
		selfUpdateURL = "http://manager"
	case !*insecure:
		if *managerFingerprint == "" {
			log.Fatal("agent: -manager-fingerprint is required unless -insecure is set (get it from the manager's startup log)")
		}
		transport = ws.NewTLSClient(mtls.PinnedClientConfig(*managerFingerprint))
	default:
		log.Println("agent: running with -insecure: plaintext transport, manager identity not verified")
		transport = ws.New()
	}

	effectiveManagerAddr := *managerAddr
	if *relayAddr != "" {
		effectiveManagerAddr = *relayAddr
	}

	a, err := agent.New(transport, agent.Config{
		ManagerAddr:               effectiveManagerAddr,
		Discoverer:                &udp.Discoverer{},
		PairingToken:              *pairingToken,
		IdentityDir:               *identityDir,
		WorkloadSlots:             *slots,
		Name:                      *name,
		HeartbeatInterval:         *heartbeatInterval,
		InsecureWorkloadsDisabled: *insecure,
		// LaunchArgs/Insecure/ManagerFingerprint exist solely for
		// selfupdate.go: a self-relaunch execs this same binary with these
		// exact flags, and the download needs to know the scheme and (if
		// not insecure) the pinned fingerprint to trust for its own
		// short-lived HTTP connection to the manager.
		LaunchArgs:           os.Args[1:],
		Insecure:             *insecure,
		ManagerFingerprint:   *managerFingerprint,
		SelfUpdateHTTPClient: selfUpdateHTTPClient,
		SelfUpdateBaseURL:    selfUpdateURL,
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// One live process per identity: a launcher restart can overlap a
	// self-update's relaunch (or someone starts a second copy by hand). A
	// second process waits here as a standby and takes over only once the
	// running one exits — never two agents with one identity.
	lock, err := instancelock.TryAcquire(*identityDir)
	if errors.Is(err, instancelock.ErrHeld) {
		log.Printf("agent: another agent with this identity is already running; waiting as a standby")
	}
	for errors.Is(err, instancelock.ErrHeld) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		lock, err = instancelock.TryAcquire(*identityDir)
	}
	if err != nil {
		log.Fatalf("agent: instance lock: %v", err)
	}
	defer lock.Release()
	defer stop()

	log.Printf("agent %s starting (identity dir: %s)", a.NodeID(), *identityDir)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("agent: %v", err)
	}
}

func defaultIdentityDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".home-harness-agent"
	}
	return filepath.Join(dir, "home-harness", "agent")
}
