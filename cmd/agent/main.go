// Command agent runs a harness node: it discovers (or is pointed at) the
// manager, registers with a persistent identity, and heartbeats until
// stopped, reconnecting automatically on any failure.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/mtls"
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
	flag.Parse()

	if *pairingToken == "" {
		log.Fatal("agent: -pairing-token is required")
	}

	transport := ws.New()
	if !*insecure {
		if *managerFingerprint == "" {
			log.Fatal("agent: -manager-fingerprint is required unless -insecure is set (get it from the manager's startup log)")
		}
		transport = ws.NewTLSClient(mtls.PinnedClientConfig(*managerFingerprint))
	} else {
		log.Println("agent: running with -insecure: plaintext transport, manager identity not verified")
	}

	a, err := agent.New(transport, agent.Config{
		ManagerAddr:       *managerAddr,
		Discoverer:        &udp.Discoverer{},
		PairingToken:      *pairingToken,
		IdentityDir:       *identityDir,
		Name:              *name,
		HeartbeatInterval: *heartbeatInterval,
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
