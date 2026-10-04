package manager

import (
	"errors"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func attempt(n int, node domain.NodeID, state domain.WorkloadState, nodeLost bool) WorkloadRecord {
	return WorkloadRecord{
		Workload: domain.Workload{ID: domain.WorkloadID(string(rune('a' + n))), Target: node, Attempt: n},
		Status:   domain.WorkloadStatus{State: state, NodeLost: nodeLost, Error: "boom"},
	}
}

func TestDeriveTaskCountsRealFailuresAndAvoidsTheirNodes(t *testing.T) {
	if tp := deriveTask(nil, 3); tp.state != taskWaiting || tp.attempts != 0 {
		t.Fatalf("no attempts: %+v", tp)
	}
	tp := deriveTask([]WorkloadRecord{attempt(1, "n1", domain.WorkloadFailed, false)}, 3)
	if tp.state != taskWaiting || tp.counted != 1 || len(tp.avoid) != 1 || tp.avoid[0] != "n1" {
		t.Fatalf("one real failure: %+v", tp)
	}
	tp = deriveTask([]WorkloadRecord{
		attempt(1, "n1", domain.WorkloadFailed, false),
		attempt(2, "n2", domain.WorkloadFailed, false),
		attempt(3, "n1", domain.WorkloadFailed, false),
	}, 3)
	if tp.state != taskFailed || tp.counted != 3 || len(tp.avoid) != 2 {
		t.Fatalf("attempts exhausted (MaxAttempts counts the first run): %+v", tp)
	}
	tp = deriveTask([]WorkloadRecord{attempt(1, "n1", domain.WorkloadRunning, false)}, 3)
	if tp.state != taskActive || tp.current == nil {
		t.Fatalf("running: %+v", tp)
	}
	tp = deriveTask([]WorkloadRecord{attempt(1, "n1", domain.WorkloadFailed, false), attempt(2, "n2", domain.WorkloadCompleted, false)}, 3)
	if tp.state != taskDone {
		t.Fatalf("completed on retry: %+v", tp)
	}
	if tp := deriveTask([]WorkloadRecord{attempt(1, "n1", domain.WorkloadCanceled, false)}, 3); tp.state != taskCanceled {
		t.Fatalf("canceled: %+v", tp)
	}
}

func TestDeriveTaskDoesNotChargeNodeLossOrRestarts(t *testing.T) {
	var attempts []WorkloadRecord
	for i := 1; i <= maxNodeLostRetries; i++ {
		state := domain.WorkloadFailed
		if i%2 == 0 {
			state = domain.WorkloadUnknown // a manager restart
		}
		attempts = append(attempts, attempt(i, "n1", state, state == domain.WorkloadFailed))
	}
	tp := deriveTask(attempts, 1)
	if tp.state != taskWaiting || tp.counted != 0 || len(tp.avoid) != 0 {
		t.Fatalf("node losses must neither count nor avoid the node: %+v", tp)
	}
	// Past the cap they count, so a task that keeps taking its device
	// down can't retry forever.
	attempts = append(attempts, attempt(maxNodeLostRetries+1, "n1", domain.WorkloadFailed, true))
	if tp := deriveTask(attempts, 1); tp.state != taskFailed {
		t.Fatalf("node losses beyond the cap must count: %+v", tp)
	}
}

func readyNode(t *testing.T, r *Registry, id domain.NodeID, slots int) {
	t.Helper()
	r.Upsert(testNode(id), &fakeConn{tag: string(id)})
	r.SetState(id, domain.NodeReady)
	r.UpdateResources(id, []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 8, Unit: "cores"}}, []domain.Capability{{Name: domain.CapabilitySystemExecute}})
	r.RecordHeartbeat(id, domain.RuntimeState{MemoryAvailableBytes: 8 << 30, LastHeartbeat: time.Now()})
	r.mu.Lock()
	r.nodes[id].slotsN = slots
	r.mu.Unlock()
}

func TestAvoidNodesPreferredButNotRequired(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 1)
	readyNode(t, r, "n2", 1)
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}
	p := placement{capability: domain.CapabilitySystemExecute, avoid: []domain.NodeID{"n1"}}
	if _, id, err := s.resolve(p, nil); err != nil || id != "n2" {
		t.Fatalf("expected n2 (n1 avoided), got %s %v", id, err)
	}
	// n2 busy: the retry waits for it rather than going back to n1.
	s.Workloads.Put(domain.Workload{ID: "busy", Target: "n2"}, domain.WorkloadStatus{State: domain.WorkloadRunning})
	if _, _, err := s.resolve(p, nil); !errors.Is(err, errNoRoom) {
		t.Fatalf("expected errNoRoom while the preferred node is busy, got %v", err)
	}
	// Only avoided nodes could ever run it: use them anyway.
	p.avoid = []domain.NodeID{"n1", "n2"}
	if _, id, err := s.resolve(p, nil); err != nil || id != "n1" {
		t.Fatalf("all nodes avoided: expected n1, got %s %v", id, err)
	}
}

// One dispatch pass against one snapshot must still respect every slot.
func TestDispatchPassNeverOversubscribes(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 2)
	readyNode(t, r, "n2", 3)
	s := newReconcileTestServer(r, NewWorkloadRegistry())
	s.grants = newGrantTable()
	now := time.Now()
	for i := 0; i < 20; i++ {
		s.Workloads.enqueue(domain.Workload{ID: domain.WorkloadID(rune('A' + i))}, now)
	}
	// Behind them, work of another capability: the early exit is per
	// capability, so it still gets n3 in the same pass.
	readyNode(t, r, "n3", 1)
	r.UpdateResources("n3", nil, []domain.Capability{{Name: domain.CapabilityFilesystemRead}})
	s.Workloads.enqueue(domain.Workload{ID: "read", Capability: domain.CapabilityFilesystemRead}, now)
	s.dispatchQueued(t.Context())
	u := s.Workloads.usageByNode()
	if u["n1"].running != 2 || u["n2"].running != 3 {
		t.Fatalf("after one pass: n1=%d n2=%d, want 2 and 3", u["n1"].running, u["n2"].running)
	}
	if got := s.Workloads.queuedCount(); got != 15 {
		t.Fatalf("queued after the pass = %d, want 15", got)
	}
	if rec, _ := s.Workloads.Get("read"); rec.Status.State != domain.WorkloadPending || rec.Workload.Target != "n3" {
		t.Fatalf("other-capability work starved by the early exit: %+v", rec.Status)
	}
}

// A running job's inputs and its finished tasks' outputs (the reduce's
// future inputs) must survive GC; a finished job's are ordinary.
func TestJobArtifactsStayLiveWhileTheJobRuns(t *testing.T) {
	s := newReconcileTestServer(NewRegistry(), NewWorkloadRegistry())
	s.jobs, s.workflows = newJobTable(), newWorkflowTable()
	in, shared, part := "aa", "bb", "cc"
	job := domain.Job{ID: "j", State: domain.JobRunning, MaxAttempts: 1,
		Tasks:  []domain.TaskSpec{{Command: "x", Inputs: []domain.ArtifactRef{{Name: "in", SHA256: in}}}},
		Reduce: &domain.TaskSpec{Command: "y", Inputs: []domain.ArtifactRef{{Name: "s", SHA256: shared}}}}
	s.jobs.put(job)
	s.Workloads.Put(domain.Workload{ID: "w", Job: "j", Task: domain.TaskKey(0), Attempt: 1},
		domain.WorkloadStatus{State: domain.WorkloadCompleted, Outputs: []domain.ArtifactRef{{Name: "out", SHA256: part}}})
	live := s.liveArtifacts()
	for _, sha := range []string{in, shared, part} {
		if !live[sha] {
			t.Fatalf("%s not live while the job runs: %v", sha, live)
		}
	}
	s.jobs.finish("j", domain.JobCompleted, "")
	if live := s.liveArtifacts(); len(live) != 0 {
		t.Fatalf("a finished job keeps artifacts live: %v", live)
	}
}

// A big job fans out over several passes, not all in one.
func TestAdvanceJobsSubmitsAtMostABudgetPerPass(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 4)
	s := newReconcileTestServer(r, NewWorkloadRegistry())
	s.grants, s.jobs, s.dispatchKick = newGrantTable(), newJobTable(), make(chan struct{}, 1)
	tasks := make([]domain.TaskSpec, maxAttemptsPerPass+50)
	for i := range tasks {
		tasks[i] = domain.TaskSpec{Command: "x"}
	}
	s.jobs.put(domain.Job{ID: "big", State: domain.JobRunning, MaxAttempts: 1, Tasks: tasks})
	attempts := func() int {
		n := 0
		for _, as := range s.Workloads.byJob()["big"] {
			n += len(as)
		}
		return n
	}
	s.advanceJobs(t.Context())
	if got := attempts(); got != maxAttemptsPerPass {
		t.Fatalf("first pass submitted %d attempts, want %d", got, maxAttemptsPerPass)
	}
	select {
	case <-s.dispatchKick:
	default:
		t.Fatal("a pass that stopped early must kick the next one")
	}
	s.advanceJobs(t.Context())
	if got := attempts(); got != len(tasks) {
		t.Fatalf("after two passes: %d attempts, want %d", got, len(tasks))
	}
}
