package manager

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/eventbus"
)

// newReconcileTestServer builds a bare Server sufficient for
// restartWorkload/reconcileOnce (Registry, Workloads, Events, cfg) without
// a real transport or persistent store — matching
// TestResolveWorkloadTargetIntegratesWithRegistry's pattern in
// placement_test.go, extended with Events since restartWorkload publishes.
func newReconcileTestServer(r *Registry, wr *WorkloadRegistry) *Server {
	return &Server{
		Registry:  r,
		Workloads: wr,
		Events:    eventbus.New(),
		cfg:       Config{ReconcileInterval: 5 * time.Second},
	}
}

func TestBackoffForDoublesAndCaps(t *testing.T) {
	base := 5 * time.Second
	max := 5 * time.Minute
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{6, 5 * time.Minute}, // 5s * 2^6 = 320s > 300s cap
		{20, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := backoffFor(c.count, base, max); got != c.want {
			t.Errorf("backoffFor(%d) = %s, want %s", c.count, got, c.want)
		}
	}
}

func TestMarkRestartingClearsStaleOutputAndIncrementsCount(t *testing.T) {
	wr := NewWorkloadRegistry()
	w := domain.Workload{ID: "w1", Target: "node-a", Command: "echo"}
	wr.Put(w, domain.WorkloadStatus{
		ID: "w1", Target: "node-a", State: domain.WorkloadFailed,
		Stdout: "old output", Stderr: "old error output", ExitCode: 1, Error: "crashed",
		StartedAt: time.Now(), FinishedAt: time.Now(),
	})

	rec, ok := wr.MarkRestarting("w1", "node-b")
	if !ok {
		t.Fatal("expected MarkRestarting to find the workload")
	}
	if rec.Status.State != domain.WorkloadPending {
		t.Fatalf("expected PENDING after restart, got %s", rec.Status.State)
	}
	if rec.Status.Stdout != "" || rec.Status.Stderr != "" || rec.Status.Error != "" || rec.Status.ExitCode != 0 {
		t.Fatalf("expected previous attempt's output cleared, got %+v", rec.Status)
	}
	if !rec.Status.StartedAt.IsZero() || !rec.Status.FinishedAt.IsZero() {
		t.Fatalf("expected timestamps cleared, got %+v", rec.Status)
	}
	if rec.Workload.Target != "node-b" {
		t.Fatalf("expected target updated to node-b, got %s", rec.Workload.Target)
	}
	if rec.Restart.Count != 1 {
		t.Fatalf("expected restart count 1, got %d", rec.Restart.Count)
	}
}

// TestMarkRestartingResetsBackoffAfterHealthyRun proves an attempt that ran
// long enough (FinishedAt - StartedAt >= restartHealthyRunThreshold) resets
// BackoffCount to 0 even though the lifetime Count keeps climbing — a
// service that was fine for hours before one crash should get a short
// backoff, not whatever a high lifetime Count would otherwise imply.
func TestMarkRestartingResetsBackoffAfterHealthyRun(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	w := domain.Workload{ID: "w1", Target: "node-a"}
	wr.Put(w, domain.WorkloadStatus{
		ID: "w1", Target: "node-a", State: domain.WorkloadFailed,
		StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-1 * time.Minute),
	})
	wr.workloads["w1"].Restart = domain.RestartState{Count: 3, BackoffCount: 6}

	rec, ok := wr.MarkRestarting("w1", "node-b")
	if !ok {
		t.Fatal("expected MarkRestarting to find the workload")
	}
	if rec.Restart.Count != 4 {
		t.Fatalf("expected lifetime count to keep climbing to 4, got %d", rec.Restart.Count)
	}
	if rec.Restart.BackoffCount != 0 {
		t.Fatalf("expected a healthy run (9m uptime) to reset BackoffCount to 0, got %d", rec.Restart.BackoffCount)
	}
}

// TestMarkRestartingIncrementsBackoffAfterShortRun is
// TestMarkRestartingResetsBackoffAfterHealthyRun's mirror: an attempt that
// crashed quickly (well under restartHealthyRunThreshold) must increment
// BackoffCount, not reset it — this is the crash-looping case the backoff
// curve exists for.
func TestMarkRestartingIncrementsBackoffAfterShortRun(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	w := domain.Workload{ID: "w1", Target: "node-a"}
	wr.Put(w, domain.WorkloadStatus{
		ID: "w1", Target: "node-a", State: domain.WorkloadFailed,
		StartedAt: now.Add(-2 * time.Second), FinishedAt: now,
	})
	wr.workloads["w1"].Restart = domain.RestartState{Count: 3, BackoffCount: 6}

	rec, ok := wr.MarkRestarting("w1", "node-b")
	if !ok {
		t.Fatal("expected MarkRestarting to find the workload")
	}
	if rec.Restart.BackoffCount != 7 {
		t.Fatalf("expected a short run (2s uptime) to increment BackoffCount to 7, got %d", rec.Restart.BackoffCount)
	}
}

func TestRestartCandidatesExcludesNotYetDue(t *testing.T) {
	wr := NewWorkloadRegistry()
	w := domain.Workload{ID: "w1", Target: "node-a", RestartPolicy: domain.RestartAlways}
	wr.Seed(domain.PersistedWorkload{
		Workload: w,
		Status:   domain.WorkloadStatus{ID: "w1", Target: "node-a", State: domain.WorkloadFailed, StartedAt: time.Now()},
		Restart:  domain.RestartState{NextRestartAt: time.Now().Add(1 * time.Hour)},
	})

	if got := wr.RestartCandidates(time.Now()); len(got) != 0 {
		t.Fatalf("expected no candidates while NextRestartAt is in the future, got %d", len(got))
	}

	if got := wr.RestartCandidates(time.Now().Add(2 * time.Hour)); len(got) != 1 {
		t.Fatalf("expected 1 candidate once NextRestartAt has passed, got %d", len(got))
	}
}

func TestRestartCandidatesRespectsPolicy(t *testing.T) {
	wr := NewWorkloadRegistry()
	wr.Seed(domain.PersistedWorkload{
		Workload: domain.Workload{ID: "never", RestartPolicy: domain.RestartNever},
		Status:   domain.WorkloadStatus{ID: "never", State: domain.WorkloadFailed, StartedAt: time.Now()},
	})
	wr.Seed(domain.PersistedWorkload{
		Workload: domain.Workload{ID: "canceled", RestartPolicy: domain.RestartAlways},
		Status:   domain.WorkloadStatus{ID: "canceled", State: domain.WorkloadCanceled},
	})
	wr.Seed(domain.PersistedWorkload{
		Workload: domain.Workload{ID: "eligible", RestartPolicy: domain.RestartAlways},
		Status:   domain.WorkloadStatus{ID: "eligible", State: domain.WorkloadFailed, StartedAt: time.Now()},
	})

	candidates := wr.RestartCandidates(time.Now())
	if len(candidates) != 1 || candidates[0].Workload.ID != "eligible" {
		t.Fatalf("expected only the RestartAlways+FAILED workload to be a candidate, got %+v", candidates)
	}
}

// TestRestartWorkloadReplacesNodeWhenOriginalGoesOffline proves an
// originally-auto-placed (not Pinned) workload is free to land on any
// currently-eligible node when its previous node is no longer READY — the
// "self-healing across the fleet" case.
func TestRestartWorkloadReplacesNodeWhenOriginalGoesOffline(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), &fakeConn{tag: "a"})
	r.SetState("node-a", domain.NodeOffline) // no longer available

	r.Upsert(testNode("node-b"), &fakeConn{tag: "b"})
	r.SetState("node-b", domain.NodeReady)

	wr := NewWorkloadRegistry()
	wr.Put(domain.Workload{ID: "w1", Target: "node-a", Pinned: false, RestartPolicy: domain.RestartAlways},
		domain.WorkloadStatus{ID: "w1", Target: "node-a", State: domain.WorkloadFailed, StartedAt: time.Now()})

	s := newReconcileTestServer(r, wr)

	rec, _ := wr.Get("w1")
	s.restartWorkload(context.Background(), rec)

	updated, ok := wr.Get("w1")
	if !ok {
		t.Fatal("expected workload to still exist")
	}
	if updated.Workload.Target != "node-b" {
		t.Fatalf("expected restart to move to node-b, got %s", updated.Workload.Target)
	}
	if updated.Status.State != domain.WorkloadPending {
		t.Fatalf("expected PENDING after restart, got %s", updated.Status.State)
	}
	if updated.Restart.Count != 1 {
		t.Fatalf("expected restart count 1, got %d", updated.Restart.Count)
	}
}

// TestRestartWorkloadDefersWhenPinnedNodeNotReady proves a Pinned workload
// stays pinned across restarts — it waits for its exact node rather than
// being reassigned elsewhere, matching v2's "explicit target is never
// silently overridden" precedent.
func TestRestartWorkloadDefersWhenPinnedNodeNotReady(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), &fakeConn{tag: "a"})
	r.SetState("node-a", domain.NodeOffline)

	wr := NewWorkloadRegistry()
	wr.Put(domain.Workload{ID: "w1", Target: "node-a", Pinned: true, RestartPolicy: domain.RestartAlways},
		domain.WorkloadStatus{ID: "w1", Target: "node-a", State: domain.WorkloadFailed, StartedAt: time.Now()})

	s := newReconcileTestServer(r, wr)

	rec, _ := wr.Get("w1")
	s.restartWorkload(context.Background(), rec)

	updated, _ := wr.Get("w1")
	if updated.Workload.Target != "node-a" {
		t.Fatalf("expected pinned target to remain node-a, got %s", updated.Workload.Target)
	}
	if updated.Status.State != domain.WorkloadFailed {
		t.Fatalf("expected status to remain FAILED (deferred, not restarted), got %s", updated.Status.State)
	}
	if updated.Restart.Count != 0 {
		t.Fatalf("expected restart count to stay 0 for a deferred attempt, got %d", updated.Restart.Count)
	}
	if !updated.Restart.NextRestartAt.After(time.Now()) {
		t.Fatal("expected NextRestartAt to be pushed into the future")
	}
}

// TestRestartWorkloadDefersPlacementRejectionWithoutCountingFailure proves
// a FAILED status with a zero StartedAt (rejected before any process
// launched — e.g. the executor was already busy) is deferred, not counted
// against the backoff curve — it was never a real crash.
func TestRestartWorkloadDefersPlacementRejectionWithoutCountingFailure(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), &fakeConn{tag: "a"})
	r.SetState("node-a", domain.NodeReady)

	wr := NewWorkloadRegistry()
	wr.Put(domain.Workload{ID: "w1", Target: "node-a", Pinned: true, RestartPolicy: domain.RestartAlways},
		domain.WorkloadStatus{ID: "w1", Target: "node-a", State: domain.WorkloadFailed}) // StartedAt left zero

	s := newReconcileTestServer(r, wr)

	rec, _ := wr.Get("w1")
	s.restartWorkload(context.Background(), rec)

	updated, _ := wr.Get("w1")
	if updated.Restart.Count != 0 {
		t.Fatalf("expected a placement-time rejection to NOT increment restart count, got %d", updated.Restart.Count)
	}
	if !updated.Restart.NextRestartAt.After(time.Now()) {
		t.Fatal("expected NextRestartAt to be pushed into the future even though it wasn't counted")
	}
}

// TestReconcileOnceRestartsEligibleWorkloads is an end-to-end exercise of
// the per-tick body against a real WorkloadRegistry/Registry pair (no real
// timers), confirming RestartCandidates -> restartWorkload wiring.
func TestReconcileOnceRestartsEligibleWorkloads(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), &fakeConn{tag: "a"})
	r.SetState("node-a", domain.NodeReady)

	wr := NewWorkloadRegistry()
	wr.Put(domain.Workload{ID: "w1", Target: "node-a", Pinned: true, RestartPolicy: domain.RestartOnFailure},
		domain.WorkloadStatus{ID: "w1", Target: "node-a", State: domain.WorkloadFailed, StartedAt: time.Now()})
	// Not restart-eligible: RestartNever.
	wr.Put(domain.Workload{ID: "w2", Target: "node-a", Pinned: true, RestartPolicy: domain.RestartNever},
		domain.WorkloadStatus{ID: "w2", Target: "node-a", State: domain.WorkloadFailed, StartedAt: time.Now()})

	s := newReconcileTestServer(r, wr)
	s.reconcileOnce(context.Background())

	w1, _ := wr.Get("w1")
	if w1.Status.State != domain.WorkloadPending {
		t.Fatalf("expected w1 (OnFailure) to be restarted, got %s", w1.Status.State)
	}
	w2, _ := wr.Get("w2")
	if w2.Status.State != domain.WorkloadFailed {
		t.Fatalf("expected w2 (Never) to be left alone, got %s", w2.Status.State)
	}
}
