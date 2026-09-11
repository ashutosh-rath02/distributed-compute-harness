package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

func TestNodeSurvivesManagerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	identityDir := filepath.Join(t.TempDir(), "agent-persist")
	const addr = "127.0.0.1:19197"

	// --- First manager instance: register a node, then shut down. ---
	store1, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("persistent.Open: %v", err)
	}

	mgrCtx1, mgrCancel1 := context.WithCancel(context.Background())
	srv1 := manager.NewServer(ws.New(), store1, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv1.Run(mgrCtx1)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "persistent-agent",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv1.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	// Shut everything down: agent disconnects, manager stops, store closes.
	agentCancel()
	mgrCancel1()
	time.Sleep(100 * time.Millisecond) // let goroutines unwind before closing the db file
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	// --- Second manager instance, same DB file, agent not yet reconnected. ---
	store2, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("persistent.Open (reopen): %v", err)
	}
	defer store2.Close()

	mgrCtx2, mgrCancel2 := context.WithCancel(context.Background())
	defer mgrCancel2()
	const addr2 = "127.0.0.1:19198"
	srv2 := manager.NewServer(ws.New(), store2, manager.Config{
		Addr:             addr2,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv2.Run(mgrCtx2)

	waitFor(t, 2*time.Second, func() bool {
		_, ok := srv2.Registry.Get(a.NodeID())
		return ok
	})

	rec, ok := srv2.Registry.Get(a.NodeID())
	if !ok {
		t.Fatal("expected node to be known to the restarted manager without reconnecting")
	}
	if rec.Node.Name != "persistent-agent" {
		t.Fatalf("expected persisted node name %q, got %q", "persistent-agent", rec.Node.Name)
	}
	if rec.State != domain.NodeOffline {
		t.Fatalf("expected a seeded, not-yet-reconnected node to be OFFLINE, got %s", rec.State)
	}

	// Now let the same agent identity reconnect to the restarted manager
	// and confirm it comes back READY under the same NodeID.
	agentCtx2, agentCancel2 := context.WithCancel(context.Background())
	defer agentCancel2()
	a2, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr2,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "persistent-agent",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New (after restart): %v", err)
	}
	if a2.NodeID() != a.NodeID() {
		t.Fatalf("expected same node id after manager restart, got %q want %q", a2.NodeID(), a.NodeID())
	}
	go a2.Run(agentCtx2)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv2.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	if got := len(srv2.Registry.List()); got != 1 {
		t.Fatalf("expected exactly 1 node after reconnect to restarted manager, got %d", got)
	}
}
