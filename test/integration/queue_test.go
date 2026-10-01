package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

func workloadState(srv *manager.Server, id domain.WorkloadID) domain.WorkloadState {
	rec, _ := srv.Workloads.Get(id)
	return rec.Status.State
}

func countStates(srv *manager.Server, ids []domain.WorkloadID) map[domain.WorkloadState]int {
	out := map[domain.WorkloadState]int{}
	for _, id := range ids {
		out[workloadState(srv, id)]++
	}
	return out
}

// A burst bigger than the fleet's slots runs as much as fits — spread so
// no node exceeds its slots — queues the rest, and starts queued work as
// soon as a slot frees.
func TestBurstFillsSlotsAcrossNodesAndQueuesTheRest(t *testing.T) {
	const addr = "127.0.0.1:19520"
	srv := startManager(t, addr, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startAgentWithSlots(t, ctx, addr, "burst-a", 2)
	b := startAgentWithSlots(t, ctx, addr, "burst-b", 2)
	waitFor(t, 5*time.Second, func() bool {
		ra, okA := srv.Registry.Get(a.NodeID())
		rb, okB := srv.Registry.Get(b.NodeID())
		return okA && okB && ra.State == domain.NodeReady && rb.State == domain.NodeReady
	})

	sleepCmd, sleepArgv := sleepArgs("30")
	var ids []domain.WorkloadID
	for i := 0; i < 6; i++ {
		wl, err := srv.SubmitWorkload(ctx, "", sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
		if err != nil {
			t.Fatalf("SubmitWorkload %d: %v", i, err)
		}
		ids = append(ids, wl.ID)
	}
	waitFor(t, 15*time.Second, func() bool {
		c := countStates(srv, ids)
		return c[domain.WorkloadRunning] == 4 && c[domain.WorkloadQueued] == 2
	})
	perNode := map[domain.NodeID]int{}
	var queued []domain.WorkloadID
	var victim domain.WorkloadID
	for _, id := range ids {
		rec, _ := srv.Workloads.Get(id)
		switch rec.Status.State {
		case domain.WorkloadRunning:
			perNode[rec.Workload.Target]++
			victim = id
		case domain.WorkloadQueued:
			queued = append(queued, id)
		}
	}
	if perNode[a.NodeID()] != 2 || perNode[b.NodeID()] != 2 {
		t.Fatalf("expected 2 running on each 2-slot node, got %v", perNode)
	}
	// The queue is FIFO: the two still waiting are the last two submitted.
	if queued[0] != ids[4] || queued[1] != ids[5] {
		t.Fatalf("expected the last two submissions queued, got %v of %v", queued, ids)
	}

	if err := srv.CancelWorkload(ctx, victim); err != nil {
		t.Fatalf("CancelWorkload: %v", err)
	}
	waitFor(t, 15*time.Second, func() bool {
		return workloadState(srv, victim) == domain.WorkloadCanceled && workloadState(srv, ids[4]) == domain.WorkloadRunning
	})
	if got := workloadState(srv, ids[5]); got != domain.WorkloadQueued {
		t.Fatalf("expected the newest submission still QUEUED (no slot for it yet), got %s", got)
	}

	// Canceling a queued workload is immediate and it never runs.
	if err := srv.CancelWorkload(ctx, ids[5]); err != nil {
		t.Fatalf("CancelWorkload (queued): %v", err)
	}
	if got := workloadState(srv, ids[5]); got != domain.WorkloadCanceled {
		t.Fatalf("expected a queued workload CANCELED at once, got %s", got)
	}
	if rec, _ := srv.Workloads.Get(ids[5]); !rec.Status.StartedAt.IsZero() {
		t.Fatal("a workload canceled while queued must never have started")
	}
	for _, id := range ids {
		_ = srv.CancelWorkload(ctx, id)
	}
}

// Requests that can't ever run are still refused up front, not queued:
// queueing them would only park work no node will take.
func TestImpossibleOrNodelessSubmissionsAreRejectedNotQueued(t *testing.T) {
	const addr = "127.0.0.1:19521"
	srv := startManager(t, addr, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sleepCmd, sleepArgv := sleepArgs("1")
	if _, err := srv.SubmitWorkload(ctx, "", sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever); err == nil {
		t.Fatal("expected a submission with no ready node to be rejected")
	}
	a := startAgentWithSlots(t, ctx, addr, "impossible-agent", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	huge := domain.ResourceRequirements{MinMemoryBytes: 1 << 50}
	if _, err := srv.SubmitWorkload(ctx, "", sleepCmd, sleepArgv, "", nil, huge, domain.RestartNever); err == nil {
		t.Fatal("expected a submission no node could ever fit to be rejected")
	}
	if _, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv, "", nil, huge, domain.RestartNever); err == nil {
		t.Fatal("expected a pinned submission its node could never fit to be rejected")
	}
	for _, rec := range srv.Workloads.List() {
		t.Fatalf("a rejected submission left a workload record behind: %+v", rec.Status)
	}
}

// A raw agent advertising 2 slots that refuses work as busy (the race
// between the manager's view of its slots and the agent's): the workload
// is re-queued — not failed, not resent at wire speed — and runs once a
// slot genuinely frees. A refusal for work that isn't waiting to start is
// ignored.
func TestBusyRefusalRequeuesWithoutSpinning(t *testing.T) {
	const addr = "127.0.0.1:19522"
	srv := startManager(t, addr, 30*time.Second)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "refusing-agent", func(m *domain.Manifest) {
		m.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
		m.WorkloadSlots = 2
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	assigns := make(chan domain.WorkloadID, 32)
	go func() {
		for {
			data, err := conn.Receive(ctx)
			if err != nil {
				return
			}
			env, err := protocol.Decode(data)
			if err != nil || env.Type != protocol.MsgWorkloadAssign {
				continue
			}
			var p protocol.WorkloadAssignPayload
			if env.DecodePayload(&p) == nil {
				assigns <- p.Workload.ID
			}
		}
	}()
	report := func(st domain.WorkloadStatus) {
		t.Helper()
		st.Target = nodeID
		env, _ := protocol.NewEnvelope(protocol.MsgWorkloadStatus, nodeID, domain.ManagerNodeID, protocol.WorkloadStatusPayload{Status: st})
		wire, _ := protocol.Encode(env)
		if err := conn.Send(ctx, wire); err != nil {
			t.Fatalf("send WORKLOAD_STATUS: %v", err)
		}
	}
	nextAssign := func(want domain.WorkloadID) {
		t.Helper()
		select {
		case got := <-assigns:
			if got != want {
				t.Fatalf("expected an ASSIGN for %s, got %s", want, got)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for an ASSIGN for %s", want)
		}
	}
	submit := func() domain.WorkloadID {
		t.Helper()
		wl, err := srv.SubmitWorkload(ctx, nodeID, "cmd", []string{"/C", "echo", "x"}, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
		if err != nil {
			t.Fatalf("SubmitWorkload: %v", err)
		}
		return wl.ID
	}

	first := submit()
	nextAssign(first)
	report(domain.WorkloadStatus{ID: first, State: domain.WorkloadRunning, StartedAt: time.Now().UTC()})
	second := submit()
	nextAssign(second)
	report(domain.WorkloadStatus{ID: second, State: domain.WorkloadFailed, Error: "busy", Retryable: true})
	waitFor(t, 3*time.Second, func() bool { return workloadState(srv, second) == domain.WorkloadQueued })

	// Held off: a third submission queues too, though the manager counts
	// only one of two slots in use, and nothing is resent meanwhile.
	third := submit()
	if got := workloadState(srv, third); got != domain.WorkloadQueued {
		t.Fatalf("expected new work QUEUED while the node is held off after a busy refusal, got %s", got)
	}
	select {
	case id := <-assigns:
		t.Fatalf("a refused workload was resent before any slot freed: %s", id)
	case <-time.After(500 * time.Millisecond):
	}

	// A stale retryable refusal for running work changes nothing.
	report(domain.WorkloadStatus{ID: first, State: domain.WorkloadFailed, Error: "busy", Retryable: true})
	time.Sleep(200 * time.Millisecond)
	if got := workloadState(srv, first); got != domain.WorkloadRunning {
		t.Fatalf("a refusal for a RUNNING workload must be ignored, got %s", got)
	}

	// The slot frees: both waiting workloads go out (the refused one may
	// trail the newer one while its own retry backoff runs out).
	report(domain.WorkloadStatus{ID: first, State: domain.WorkloadCompleted, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()})
	got := map[domain.WorkloadID]bool{}
	for len(got) < 2 {
		select {
		case id := <-assigns:
			if got[id] || (id != second && id != third) {
				t.Fatalf("unexpected or repeated ASSIGN %s", id)
			}
			got[id] = true
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out waiting for both queued workloads to be assigned, got %v", got)
		}
	}

	// A non-retryable pre-start failure is a real failure, not re-queued.
	report(domain.WorkloadStatus{ID: third, State: domain.WorkloadFailed, Error: "refused for another reason"})
	waitFor(t, 3*time.Second, func() bool { return workloadState(srv, third) == domain.WorkloadFailed })
}

// The refusal can cross the node's own completion report on the wire: the
// node then has nothing running but was just marked busy. The hold-off
// must expire on its own, or that node would never be tried again.
func TestBusyHoldOffExpiresWithoutAFinishedWorkload(t *testing.T) {
	const addr = "127.0.0.1:19523"
	srv := startManager(t, addr, 30*time.Second)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "crossed-agent", func(m *domain.Manifest) {
		m.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	assigns := make(chan domain.WorkloadID, 8)
	go func() {
		for {
			data, err := conn.Receive(ctx)
			if err != nil {
				return
			}
			if env, err := protocol.Decode(data); err == nil && env.Type == protocol.MsgWorkloadAssign {
				var p protocol.WorkloadAssignPayload
				if env.DecodePayload(&p) == nil {
					assigns <- p.Workload.ID
				}
			}
		}
	}()

	wl, err := srv.SubmitWorkload(ctx, nodeID, "cmd", []string{"/C", "echo", "x"}, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}
	<-assigns
	env, _ := protocol.NewEnvelope(protocol.MsgWorkloadStatus, nodeID, domain.ManagerNodeID, protocol.WorkloadStatusPayload{
		Status: domain.WorkloadStatus{ID: wl.ID, Target: nodeID, State: domain.WorkloadFailed, Error: "busy", Retryable: true},
	})
	wire, _ := protocol.Encode(env)
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-assigns:
		if id != wl.ID {
			t.Fatalf("unexpected ASSIGN %s", id)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the refused workload was never retried on an otherwise idle node")
	}
}

// QUEUED is durable: after a manager restart the queue is intact and
// drains once the node reconnects.
func TestQueuedWorkloadSurvivesManagerRestart(t *testing.T) {
	const addr = "127.0.0.1:19524"
	dbPath := filepath.Join(t.TempDir(), "harness-queue.db")
	start := func() (*manager.Server, *persistent.Store, context.CancelFunc) {
		store, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatalf("persistent.Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		srv := manager.NewServer(ws.New(), store, manager.Config{
			Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, ReconcileInterval: 100 * time.Millisecond,
		})
		go srv.Run(ctx)
		return srv, store, cancel
	}
	srv1, store1, stop1 := start()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startAgentWithSlots(t, ctx, addr, "restart-queue-agent", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv1.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	sleepCmd, sleepArgv := sleepArgs("30")
	busy, err := srv1.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload (busy): %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return workloadState(srv1, busy.ID) == domain.WorkloadRunning })
	echoCmd, echoArgv := echoArgs("after-restart")
	waiting, err := srv1.SubmitWorkload(ctx, a.NodeID(), echoCmd, echoArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload (waiting): %v", err)
	}
	if got := workloadState(srv1, waiting.ID); got != domain.WorkloadQueued {
		t.Fatalf("expected QUEUED behind the busy slot, got %s", got)
	}

	stop1()
	time.Sleep(100 * time.Millisecond)
	store1.Close()

	srv2, store2, stop2 := start()
	defer func() { stop2(); time.Sleep(50 * time.Millisecond); store2.Close() }()
	waitFor(t, 3*time.Second, func() bool { _, ok := srv2.Workloads.Get(waiting.ID); return ok })
	switch got := workloadState(srv2, waiting.ID); got {
	case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning, domain.WorkloadCompleted:
	default:
		t.Fatalf("expected the queued workload to survive the restart still queued (or already dispatched), got %s", got)
	}
	// A manager shutting down is not the node failing: work that was in
	// flight comes back UNKNOWN, never recorded as a failure.
	if got := workloadState(srv2, busy.ID); got != domain.WorkloadUnknown {
		t.Fatalf("expected the interrupted workload UNKNOWN after a manager restart, got %s", got)
	}
	// The agent reconnects; the restart ended the busy one (the agent
	// cancels its work on disconnect), so the queued one runs.
	waitFor(t, 30*time.Second, func() bool { return workloadState(srv2, waiting.ID) == domain.WorkloadCompleted })
}

// Revoking a node cancels work queued for it specifically (pinned): it
// could never run anywhere else.
func TestRevokeCancelsPinnedQueuedWorkload(t *testing.T) {
	const addr = "127.0.0.1:19525"
	srv := startManager(t, addr, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := startAgentWithSlots(t, ctx, addr, "revoke-queue-agent", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	sleepCmd, sleepArgv := sleepArgs("30")
	if _, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever); err != nil {
		t.Fatalf("SubmitWorkload (busy): %v", err)
	}
	waiting, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload (waiting): %v", err)
	}
	if got := workloadState(srv, waiting.ID); got != domain.WorkloadQueued {
		t.Fatalf("expected QUEUED, got %s", got)
	}
	if _, err := srv.RevokeNode(ctx, a.NodeID()); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if got := workloadState(srv, waiting.ID); got != domain.WorkloadCanceled {
		t.Fatalf("expected the pinned QUEUED workload CANCELED by revocation, got %s", got)
	}
}
