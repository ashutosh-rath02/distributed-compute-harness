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
	"home-harness/internal/transport/ws"
)

func main() {
	managerAddr := flag.String("manager-addr", "", "manager address host:port; if empty, discover it via LAN multicast")
	pairingToken := flag.String("pairing-token", "", "shared secret required to register (required)")
	identityDir := flag.String("identity-dir", defaultIdentityDir(), "directory holding this node's persistent identity")
	name := flag.String("name", "", "friendly node name (defaults to hostname)")
	heartbeatInterval := flag.Duration("heartbeat-interval", 5*time.Second, "how often to send heartbeats")
	flag.Parse()

	if *pairingToken == "" {
		log.Fatal("agent: -pairing-token is required")
	}

	a, err := agent.New(ws.New(), agent.Config{
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
