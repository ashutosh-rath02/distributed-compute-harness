package integration

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// startManagerWithReconcile is startManager plus a short, test-friendly
// ReconcileInterval — the default (5s) would make these tests needlessly
// slow. A separate helper rather than changing startManager's signature,
// which every other test file in this package already calls with 2 args.
func startManagerWithReconcile(t *testing.T, addr string, heartbeatTimeout, reconcileInterval time.Duration) *manager.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := manager.NewServer(ws.New(), nil, manager.Config{
		Addr:              addr,
		PairingToken:      pairingToken,
		HeartbeatTimeout:  heartbeatTimeout,
		ReconcileInterval: reconcileInterval,
	})
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()
	return srv
}

// TestRestartAlwaysRestartsAfterCleanExit proves v3's reconciliation loop
// actually restarts a workload after it exits cleanly (RestartAlways
// restarts on ANY terminal state except CANCELED, including a clean exit)
// — entirely on its own, without any new API call.
func TestRestartAlwaysRestartsAfterCleanExit(t *testing.T) {
	const addr = "127.0.0.1:19270"
	srv := startManagerWithReconcile(t, addr, 2*time.Second, 150*time.Millisecond)
	a := startRegisteredAgent(t, addr, "restart-agent-a")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := echoArgs("restart-me")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args, "", nil, domain.ResourceRequirements{}, domain.RestartAlways)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadCompleted
	})

	// The reconciler should pick this up on its own within a couple of
	// ticks and restart it — no new SubmitWorkload call from the test.
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Restart.Count >= 1
	})
}

// TestRestartOnFailureRestartsAfterRealCrash is the feature's headline use
// case end to end: "restart my service when it crashes." It's also the one
// terminal state none of the other restart tests exercise (they route
// through COMPLETED, CANCELED, or UNKNOWN) — a real non-zero exit, through
// restartWorkload's own guard that a FAILED status with a zero StartedAt is
// a placement-time rejection, not a crash, and must NOT be counted. Since
// the executor always carries StartedAt into its terminal status for an
// actually-launched process, a real crash must be counted, and this proves
// Restart.Count actually climbs for it rather than getting stuck deferred
// forever.
func TestRestartOnFailureRestartsAfterRealCrash(t *testing.T) {
	const addr = "127.0.0.1:19271"
	srv := startManagerWithReconcile(t, addr, 2*time.Second, 150*time.Millisecond)
	a := startRegisteredAgent(t, addr, "restart-agent-fail")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := failArgs()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args, "", nil, domain.ResourceRequirements{}, domain.RestartOnFailure)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadFailed
	})
	if rec, _ := srv.Workloads.Get(wl.ID); rec.Status.StartedAt.IsZero() {
		t.Fatal("expected a real crash to have a non-zero StartedAt (sanity check on the test itself)")
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Restart.Count >= 1
	})
}

// TestCanceledWorkloadIsNeverRestarted proves CANCELED is v3's answer to
// "stop a service permanently": even under RestartAlways, a canceled
// workload must never be picked up again by the reconciler, across
// several reconcile ticks.
func TestCanceledWorkloadIsNeverRestarted(t *testing.T) {
	const addr = "127.0.0.1:19272"
	srv := startManagerWithReconcile(t, addr, 2*time.Second, 150*time.Millisecond)
	a := startRegisteredAgent(t, addr, "restart-agent-cancel")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := sleepArgs("30")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args, "", nil, domain.ResourceRequirements{}, domain.RestartAlways)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadRunning
	})

	if err := srv.CancelWorkload(ctx, wl.ID); err != nil {
		t.Fatalf("CancelWorkload: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadCanceled
	})

	// Let several reconcile ticks pass (150ms interval) — it must stay
	// CANCELED with a restart count that never moves off zero.
	time.Sleep(600 * time.Millisecond)
	rec, _ := srv.Workloads.Get(wl.ID)
	if rec.Status.State != domain.WorkloadCanceled {
		t.Fatalf("expected a canceled workload to stay CANCELED, got %s", rec.Status.State)
	}
	if rec.Restart.Count != 0 {
		t.Fatalf("expected a canceled workload never to be restarted, got restart count %d", rec.Restart.Count)
	}
}

// TestHeartbeatTimeoutSendsBestEffortCancel proves the fix for the
// heartbeat-timeout double-execution gap: a node that's connected but has
// stopped heartbeating (not actually disconnected) still gets sent
// WORKLOAD_CANCEL over its still-open connection once ExpireStale fires —
// an attempt to actually stop the process the manager is about to mark
// FAILED (and which a restart policy might place on another node), not
// just a local bookkeeping change.
func TestHeartbeatTimeoutSendsBestEffortCancel(t *testing.T) {
	const addr = "127.0.0.1:19273"
	srv := startManagerWithReconcile(t, addr, 300*time.Millisecond, 5*time.Second)

	nodeID, conn := registerRawSilentNode(t, addr, "slow-heartbeat-agent")
	defer conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := srv.SubmitWorkload(ctx, nodeID, "sleep", []string{"30"}, "", nil, domain.ResourceRequirements{}, domain.RestartNever); err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	// Drain the WORKLOAD_ASSIGN itself — the raw node never replies to it
	// (never reaches RUNNING), so from the manager's perspective this
	// workload is still PENDING when the heartbeat timeout later fires.
	assignCtx, assignCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer assignCancel()
	if _, err := conn.Receive(assignCtx); err != nil {
		t.Fatalf("expected to receive the WORKLOAD_ASSIGN: %v", err)
	}

	// The raw node never heartbeats again, so ExpireStale will fire on
	// HeartbeatTimeout — but the connection itself stays open the whole
	// time, so a WORKLOAD_CANCEL sent over it should actually arrive.
	recvCtx, recvCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer recvCancel()
	data, err := conn.Receive(recvCtx)
	if err != nil {
		t.Fatalf("expected to receive a message on the still-open connection: %v", err)
	}
	env, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Type != protocol.MsgWorkloadCancel {
		t.Fatalf("expected WORKLOAD_CANCEL sent over the still-live connection, got %s", env.Type)
	}
}

// A node marked OFFLINE by a heartbeat timeout while its connection stays
// up (seen live: the manager's laptop slept, and its own agent's local
// connection survived) could never become READY again, since only a
// REGISTER does that. The manager now drops such a connection, so a live
// agent reconnects.
func TestHeartbeatTimeoutClosesTheConnection(t *testing.T) {
	const addr = "127.0.0.1:19279"
	srv := startManagerWithReconcile(t, addr, 300*time.Millisecond, 5*time.Second)
	nodeID, conn := registerRawSilentNode(t, addr, "slept-agent")
	defer conn.Close()
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})
	// Silent from here on: the manager must time it out and hang up.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, err := conn.Receive(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the manager kept the timed-out node's connection open")
			}
			break
		}
	}
	if rec, _ := srv.Registry.Get(nodeID); rec.State != domain.NodeOffline {
		t.Fatalf("state %s, want OFFLINE", rec.State)
	}
}
