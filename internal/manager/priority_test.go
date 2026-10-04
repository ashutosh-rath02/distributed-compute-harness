package manager

import (
	"errors"
	"slices"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func queuedIDs(recs []WorkloadRecord) []domain.WorkloadID {
	var ids []domain.WorkloadID
	for _, r := range recs {
		ids = append(ids, r.Workload.ID)
	}
	return ids
}

func putQueued(wr *WorkloadRegistry, id domain.WorkloadID, p domain.Priority, queuedAt time.Time) {
	wr.Put(domain.Workload{ID: id, Priority: p}, domain.WorkloadStatus{ID: id, State: domain.WorkloadQueued, QueuedAt: queuedAt})
}

// Higher priority first; within one priority, the oldest first. A record
// from before priorities (empty) is normal.
func TestQueuedServesHigherPriorityFirstAndOldestFirstWithinOne(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	putQueued(wr, "low-old", domain.PriorityLow, now.Add(-5*time.Second))
	putQueued(wr, "legacy-normal", "", now.Add(-4*time.Second))
	putQueued(wr, "normal", domain.PriorityNormal, now.Add(-3*time.Second))
	putQueued(wr, "low-new", domain.PriorityLow, now.Add(-2*time.Second))
	putQueued(wr, "high-new", domain.PriorityHigh, now.Add(-1*time.Second))
	putQueued(wr, "high-backing-off", domain.PriorityHigh, now.Add(-9*time.Second))
	wr.workloads["high-backing-off"].Status.NotBefore = now.Add(time.Minute)

	got := queuedIDs(wr.queued(now))
	want := []domain.WorkloadID{"high-new", "legacy-normal", "normal", "low-old", "low-new"}
	if !slices.Equal(got, want) {
		t.Fatalf("queue order %v, want %v", got, want)
	}
}

// Waiting raises a workload one level per priorityAgingStep, up to high,
// so a stream of newer high-priority work can't starve low-priority work.
func TestAgingRaisesWaitingWorkOneLevelPerStepUpToHigh(t *testing.T) {
	now := time.Now()
	step := priorityAgingStep
	cases := []struct {
		p      domain.Priority
		waited time.Duration
		want   int
	}{
		{domain.PriorityLow, 0, 0},
		{domain.PriorityLow, step - time.Second, 0},
		{domain.PriorityLow, step, 1},
		{domain.PriorityLow, 2 * step, 2},
		{domain.PriorityLow, 10 * step, 2}, // never above high
		{domain.PriorityNormal, step, 2},
		{domain.PriorityHigh, 5 * step, 2},
		{"", 0, 1},
		{domain.PriorityLow, -time.Hour, 0}, // a clock that went back
	}
	for _, c := range cases {
		if got := effectiveRank(domain.Workload{Priority: c.p}, now.Add(-c.waited), now); got != c.want {
			t.Errorf("%q waiting %v: rank %d, want %d", c.p, c.waited, got, c.want)
		}
	}
	if got := effectiveRank(domain.Workload{Priority: domain.PriorityLow}, time.Time{}, now); got != 0 {
		t.Errorf("no QueuedAt: rank %d, want the base 0", got)
	}

	// In the queue: low work that waited two steps competes with new high
	// work by age, and goes first; normal work queued just now waits.
	wr := NewWorkloadRegistry()
	putQueued(wr, "normal", domain.PriorityNormal, now.Add(-time.Second))
	putQueued(wr, "high", domain.PriorityHigh, now.Add(-2*time.Second))
	putQueued(wr, "low", domain.PriorityLow, now.Add(-2*step-time.Second))
	if got := queuedIDs(wr.queued(now)); !slices.Equal(got, []domain.WorkloadID{"low", "high", "normal"}) {
		t.Fatalf("with aging: %v, want [low high normal]", got)
	}
	// Without the wait (the same queue seen a second after the low work
	// arrived) it comes last.
	if got := queuedIDs(wr.queued(now.Add(-2 * step))); !slices.Equal(got, []domain.WorkloadID{"high", "normal", "low"}) {
		t.Fatalf("before aging: %v, want [high normal low]", got)
	}
}

// A busy refusal puts work back in its place: same priority, same age.
func TestRequeueKeepsPriorityAndPlace(t *testing.T) {
	wr := NewWorkloadRegistry()
	queuedAt := time.Now().Add(-time.Minute)
	wr.Put(domain.Workload{ID: "w", Target: "n", Priority: domain.PriorityLow},
		domain.WorkloadStatus{ID: "w", Target: "n", State: domain.WorkloadPending, QueuedAt: queuedAt})
	rec, ok := wr.requeue("w", time.Now(), 0)
	if !ok || rec.Status.State != domain.WorkloadQueued {
		t.Fatalf("requeue: %v %+v", ok, rec.Status)
	}
	if rec.Workload.Priority != domain.PriorityLow || !rec.Status.QueuedAt.Equal(queuedAt) {
		t.Fatalf("requeued with priority %q queued at %v, want low at %v", rec.Workload.Priority, rec.Status.QueuedAt, queuedAt)
	}
}

// One dispatch pass hands the only free slot to the highest priority.
func TestDispatchPassStartsTheHighestPriorityFirst(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 1)
	s := newReconcileTestServer(r, NewWorkloadRegistry())
	s.grants = newGrantTable()
	now := time.Now()
	for _, w := range []domain.Workload{{ID: "low", Priority: domain.PriorityLow}, {ID: "normal"}, {ID: "high", Priority: domain.PriorityHigh}} {
		s.Workloads.enqueue(w, now)
	}
	s.dispatchQueued(t.Context())
	if rec, _ := s.Workloads.Get("high"); rec.Status.State != domain.WorkloadPending {
		t.Fatalf("the newest but highest-priority workload should have the slot: %s", rec.Status.State)
	}
	for _, id := range []domain.WorkloadID{"low", "normal"} {
		if rec, _ := s.Workloads.Get(id); rec.Status.State != domain.WorkloadQueued {
			t.Fatalf("%s: %s, want QUEUED", id, rec.Status.State)
		}
	}
}

// Submit and SubmitJob take high/normal/low, default to normal, and
// refuse anything else as an invalid request.
func TestSubmitValidatesAndDefaultsPriority(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 4)
	s := newReconcileTestServer(r, NewWorkloadRegistry())
	s.grants, s.jobs, s.dispatchKick = newGrantTable(), newJobTable(), make(chan struct{}, 1)

	if _, err := s.Submit(t.Context(), WorkloadSpec{Command: "x", Priority: "urgent"}); !errors.Is(err, ErrInvalidWorkload) {
		t.Fatalf("unknown priority: %v, want ErrInvalidWorkload", err)
	}
	w, err := s.Submit(t.Context(), WorkloadSpec{Command: "x"})
	if err != nil || w.Priority != domain.PriorityNormal {
		t.Fatalf("default priority %q (%v), want normal", w.Priority, err)
	}
	if w, err = s.Submit(t.Context(), WorkloadSpec{Command: "x", Priority: "High"}); err != nil || w.Priority != domain.PriorityHigh {
		t.Fatalf("priority %q (%v), want high", w.Priority, err)
	}

	if _, err := s.SubmitJob(t.Context(), JobSpec{Tasks: []domain.TaskSpec{{Command: "x"}}, Priority: "soon"}); !errors.Is(err, ErrInvalidWorkload) {
		t.Fatalf("job with unknown priority: %v", err)
	}
	// What the planner submits on approval: no priority, so normal.
	job, err := s.SubmitJob(t.Context(), JobSpec{Tasks: []domain.TaskSpec{{Command: "x"}}})
	if err != nil || job.Priority != domain.PriorityNormal {
		t.Fatalf("job priority %q (%v), want normal", job.Priority, err)
	}
	low, err := s.SubmitJob(t.Context(), JobSpec{Tasks: []domain.TaskSpec{{Command: "x"}, {Command: "y"}}, Priority: domain.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	s.advanceJobs(t.Context())
	for _, attempts := range s.Workloads.byJob()[low.ID] {
		for _, a := range attempts {
			if a.Workload.Priority != domain.PriorityLow {
				t.Fatalf("a low job's attempt has priority %q", a.Workload.Priority)
			}
		}
	}
	if n := len(s.Workloads.byJob()[low.ID]); n != 2 {
		t.Fatalf("%d tasks submitted, want 2", n)
	}
}
