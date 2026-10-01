package manager

import (
	"errors"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func putOn(wr *WorkloadRegistry, id domain.WorkloadID, node domain.NodeID, state domain.WorkloadState, req domain.ResourceRequirements) {
	wr.Put(domain.Workload{ID: id, Target: node, Requirements: req}, domain.WorkloadStatus{ID: id, Target: node, State: state})
}

func TestUsageCountsOnlyInFlightWorkOnThatNode(t *testing.T) {
	wr := NewWorkloadRegistry()
	req := domain.ResourceRequirements{MinCPUCores: 1, MinMemoryBytes: 1 << 30}
	putOn(wr, "p", "n1", domain.WorkloadPending, req)
	putOn(wr, "r", "n1", domain.WorkloadRunning, req)
	for i, st := range []domain.WorkloadState{domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown, domain.WorkloadQueued} {
		putOn(wr, domain.WorkloadID("done-"+string(rune('a'+i))), "n1", st, req)
	}
	putOn(wr, "other", "n2", domain.WorkloadRunning, req)

	u := wr.usageOn("n1")
	if u.running != 2 || u.cores != 2 || u.memory != 2<<30 {
		t.Fatalf("usageOn(n1) = %+v, want 2 running, 2 cores, 2 GiB", u)
	}
}

func TestHasRoomEnforcesSlotsCoresAndMemory(t *testing.T) {
	rec := nodeWithProfile("n", 4, 8<<30, 10, true)

	// Legacy manifest (no slots advertised) = one at a time.
	if ok, _ := hasRoom(rec, nodeUsage{running: 1}, domain.ResourceRequirements{}); ok {
		t.Fatal("a node advertising no slots must take one workload at a time")
	}
	rec.slotsN = (domain.Manifest{}).Slots()
	if rec.slots() != 1 {
		t.Fatalf("absent WorkloadSlots must mean 1, got %d", rec.slots())
	}

	rec.slotsN = 3
	if ok, _ := hasRoom(rec, nodeUsage{running: 2}, domain.ResourceRequirements{}); !ok {
		t.Fatal("2 of 3 slots in use: expected room")
	}
	if ok, _ := hasRoom(rec, nodeUsage{running: 3}, domain.ResourceRequirements{}); ok {
		t.Fatal("3 of 3 slots in use: expected no room")
	}
	if ok, _ := hasRoom(rec, nodeUsage{running: 1, cores: 3}, domain.ResourceRequirements{MinCPUCores: 2}); ok {
		t.Fatal("3 of 4 cores reserved, 2 more requested: expected no room")
	}
	if ok, _ := hasRoom(rec, nodeUsage{running: 1, cores: 2}, domain.ResourceRequirements{MinCPUCores: 2}); !ok {
		t.Fatal("2 of 4 cores reserved, 2 more requested: expected room")
	}
	if ok, _ := hasRoom(rec, nodeUsage{running: 1, memory: 7 << 30}, domain.ResourceRequirements{MinMemoryBytes: 2 << 30}); ok {
		t.Fatal("7 of 8 GiB reserved, 2 more requested: expected no room")
	}

	rec.busyUntil = time.Now().Add(time.Minute)
	if ok, _ := hasRoom(rec, nodeUsage{}, domain.ResourceRequirements{}); ok {
		t.Fatal("a node held off after a busy refusal must get no new work")
	}
	rec.busyUntil = time.Now().Add(-time.Second)
	if ok, _ := hasRoom(rec, nodeUsage{}, domain.ResourceRequirements{}); !ok {
		t.Fatal("an expired busy hold-off must not block placement")
	}
}

func TestQueuedIsFIFOAndRespectsNotBefore(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	add := func(id domain.WorkloadID, queuedAt, notBefore time.Time) {
		wr.Put(domain.Workload{ID: id}, domain.WorkloadStatus{ID: id, State: domain.WorkloadQueued, QueuedAt: queuedAt, NotBefore: notBefore})
	}
	add("c", now.Add(-1*time.Second), time.Time{})
	add("a", now.Add(-3*time.Second), time.Time{})
	add("b", now.Add(-2*time.Second), now.Add(time.Minute)) // backing off
	putOn(wr, "running", "n", domain.WorkloadRunning, domain.ResourceRequirements{})

	got := wr.queued(now)
	if len(got) != 2 || got[0].Workload.ID != "a" || got[1].Workload.ID != "c" {
		ids := []domain.WorkloadID{}
		for _, r := range got {
			ids = append(ids, r.Workload.ID)
		}
		t.Fatalf("queued() = %v, want [a c] (oldest first, b still backing off)", ids)
	}
	if wr.queuedCount() != 3 {
		t.Fatalf("queuedCount() = %d, want 3 (backing-off work is still queued)", wr.queuedCount())
	}
}

// Submissions in the same clock tick must still dispatch in arrival
// order, not by (random) ID.
func TestEnqueueKeepsArrivalOrderWithinOneClockTick(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	for _, id := range []domain.WorkloadID{"z", "m", "a"} {
		wr.enqueue(domain.Workload{ID: id}, now)
	}
	got := wr.queued(now.Add(time.Second))
	if len(got) != 3 || got[0].Workload.ID != "z" || got[1].Workload.ID != "m" || got[2].Workload.ID != "a" {
		t.Fatalf("queued() not in arrival order: %v %v %v", got[0].Workload.ID, got[1].Workload.ID, got[2].Workload.ID)
	}
}

func TestRequeueOnlyFromPendingAndHonorsCancel(t *testing.T) {
	wr := NewWorkloadRegistry()
	now := time.Now()
	putOn(wr, "running", "n", domain.WorkloadRunning, domain.ResourceRequirements{})
	if _, ok := wr.requeue("running", now, 0); ok {
		t.Fatal("a RUNNING workload must not be re-queued by a refusal")
	}
	putOn(wr, "done", "n", domain.WorkloadCompleted, domain.ResourceRequirements{})
	if _, ok := wr.requeue("done", now, 0); ok {
		t.Fatal("a finished workload must not be resurrected by a refusal")
	}

	putOn(wr, "pending", "n", domain.WorkloadPending, domain.ResourceRequirements{})
	rec, ok := wr.requeue("pending", now, 2)
	if !ok || rec.Status.State != domain.WorkloadQueued || rec.Workload.Target != "" {
		t.Fatalf("an unpinned refused workload must be QUEUED with no target, got ok=%v %+v", ok, rec.Status)
	}
	if !rec.Status.NotBefore.After(now) {
		t.Fatal("a re-queued workload must back off before its next attempt")
	}

	wr.Put(domain.Workload{ID: "pinned", Target: "n", Pinned: true}, domain.WorkloadStatus{ID: "pinned", Target: "n", State: domain.WorkloadPending})
	if rec, _ := wr.requeue("pinned", now, 0); rec.Workload.Target != "n" {
		t.Fatalf("a pinned workload must keep its target when re-queued, got %q", rec.Workload.Target)
	}

	putOn(wr, "canceled", "n", domain.WorkloadPending, domain.ResourceRequirements{})
	wr.markCancelRequested("canceled")
	if rec, ok := wr.requeue("canceled", now, 0); !ok || rec.Status.State != domain.WorkloadCanceled {
		t.Fatalf("an operator cancel racing a busy refusal must win, got %+v", rec.Status)
	}
}

// Live figures reflect what is running now: a node that is merely busy
// (low free memory, high CPU, or not yet heartbeated) queues the work,
// and only declared capacity decides that it can never run.
func TestBusyLiveStateQueuesButDeclaredCapacityRejects(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("n"), &fakeConn{tag: "n"})
	r.SetState("n", domain.NodeReady)
	r.UpdateResources("n", []domain.Resource{
		{Kind: domain.ResourceCPUCores, Capacity: 4, Unit: "cores"},
		{Kind: domain.ResourceMemoryBytes, Capacity: 8 << 30, Unit: "bytes"},
	}, []domain.Capability{{Name: domain.CapabilitySystemExecute}})
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}

	for _, pin := range []domain.NodeID{"", "n"} {
		if _, _, err := s.resolveWorkloadTarget(pin, domain.CapabilitySystemExecute, domain.ResourceRequirements{MinMemoryBytes: 1 << 30}); !errors.Is(err, errNoRoom) {
			t.Fatalf("pin=%q, no heartbeat yet: expected errNoRoom (wait for one), got %v", pin, err)
		}
	}
	r.RecordHeartbeat("n", domain.RuntimeState{MemoryAvailableBytes: 1 << 30, CPUPercent: 90, LastHeartbeat: time.Now()})
	for _, pin := range []domain.NodeID{"", "n"} {
		if _, _, err := s.resolveWorkloadTarget(pin, domain.CapabilitySystemExecute, domain.ResourceRequirements{MinMemoryBytes: 4 << 30}); !errors.Is(err, errNoRoom) {
			t.Fatalf("pin=%q, 4 GiB wanted, 1 GiB free of 8: expected errNoRoom, got %v", pin, err)
		}
		if _, _, err := s.resolveWorkloadTarget(pin, domain.CapabilitySystemExecute, domain.ResourceRequirements{MaxCPUPercent: 50}); !errors.Is(err, errNoRoom) {
			t.Fatalf("pin=%q, CPU at 90%% with a 50%% ceiling: expected errNoRoom, got %v", pin, err)
		}
		if _, _, err := s.resolveWorkloadTarget(pin, domain.CapabilitySystemExecute, domain.ResourceRequirements{MinMemoryBytes: 16 << 30}); !errors.Is(err, ErrNoEligibleNode) {
			t.Fatalf("pin=%q, 16 GiB wanted of 8 in total: expected ErrNoEligibleNode, got %v", pin, err)
		}
		if _, _, err := s.resolveWorkloadTarget(pin, domain.CapabilitySystemExecute, domain.ResourceRequirements{MinCPUCores: 8}); !errors.Is(err, ErrNoEligibleNode) {
			t.Fatalf("pin=%q, 8 cores wanted of 4: expected ErrNoEligibleNode, got %v", pin, err)
		}
	}
}
