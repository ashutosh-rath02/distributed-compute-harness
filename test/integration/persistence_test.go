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

// TestRunningWorkloadBecomesUnknownAfterManagerRestart proves v1's
// declared non-goal: a workload that was RUNNING (or still PENDING) the
// last time its status was persisted has its true outcome marked UNKNOWN
// on the next manager startup, not silently kept RUNNING forever and not
// guessed at — v1 does not reconcile with the agent's actual state (that
// is v3 scope).
//
// This writes the persisted record directly rather than driving it through
// a live node/connection: a real disconnect (closing the connection,
// canceling the manager's context, which unblocks that connection's
// Receive) already resolves the workload to FAILED via
// failWorkloadsFor — the exact mechanism proven in
// TestWorkloadFailsWhenNodeDisconnectsMidRun. The UNKNOWN path is for the
// other case that mechanism cannot see: the manager process itself dies
// (power loss, kill -9) without any of its own cleanup running at all, so
// whatever was last durably persisted is genuinely all that's known.
func TestRunningWorkloadBecomesUnknownAfterManagerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "harness-workload.db")

	store1, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("persistent.Open: %v", err)
	}
	wl := domain.Workload{ID: "wl-orphaned", Target: "node-orphaned", Command: "sleep", Args: []string{"30"}}
	pending := domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, State: domain.WorkloadRunning}
	if err := store1.UpsertWorkload(domain.PersistedWorkload{Workload: wl, Status: pending}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	store2, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("persistent.Open (reopen): %v", err)
	}
	defer store2.Close()

	const addr2 = "127.0.0.1:19200"
	mgrCtx2, mgrCancel2 := context.WithCancel(context.Background())
	defer mgrCancel2()
	srv2 := manager.NewServer(ws.New(), store2, manager.Config{
		Addr:             addr2,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv2.Run(mgrCtx2)

	waitFor(t, 2*time.Second, func() bool {
		_, ok := srv2.Workloads.Get(wl.ID)
		return ok
	})

	rec, ok := srv2.Workloads.Get(wl.ID)
	if !ok {
		t.Fatal("expected workload to survive manager restart")
	}
	if rec.Status.State != domain.WorkloadUnknown {
		t.Fatalf("expected UNKNOWN for an in-flight workload after restart, got %s", rec.Status.State)
	}
}
