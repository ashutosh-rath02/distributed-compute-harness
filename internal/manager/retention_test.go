package manager

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/store/persistent"
)

func finishedWorkload(wr *WorkloadRegistry, id string, capability domain.CapabilityName, state domain.WorkloadState, at time.Time) {
	wr.Put(domain.Workload{ID: domain.WorkloadID(id), Capability: capability}, domain.WorkloadStatus{ID: domain.WorkloadID(id), State: state, FinishedAt: at})
}

func TestFinishedWorkloadsExpireByAgeAndCount(t *testing.T) {
	now := time.Now()
	wr := NewWorkloadRegistry()
	finishedWorkload(wr, "old", "file.hash", domain.WorkloadCompleted, now.Add(-8*24*time.Hour))
	finishedWorkload(wr, "recent", "file.hash", domain.WorkloadFailed, now.Add(-time.Hour))
	wr.Put(domain.Workload{ID: "running"}, domain.WorkloadStatus{ID: "running", State: domain.WorkloadRunning})
	wr.Put(domain.Workload{ID: "queued"}, domain.WorkloadStatus{ID: "queued", State: domain.WorkloadQueued})
	// A job's attempts and a workload its restart policy will run again stay.
	wr.Put(domain.Workload{ID: "attempt", Job: "j1", Task: "t1"}, domain.WorkloadStatus{ID: "attempt", State: domain.WorkloadCompleted, FinishedAt: now.Add(-30 * 24 * time.Hour)})
	wr.Put(domain.Workload{ID: "service", RestartPolicy: domain.RestartAlways}, domain.WorkloadStatus{ID: "service", State: domain.WorkloadCompleted, FinishedAt: now.Add(-30 * 24 * time.Hour)})
	// Canceled while queued: no finish time, so its age starts now.
	wr.Put(domain.Workload{ID: "no-time"}, domain.WorkloadStatus{ID: "no-time", State: domain.WorkloadCanceled})

	expired := wr.expireFinished(now, finishedRetention, 10, 5)
	if len(expired) != 1 || expired[0] != "old" {
		t.Fatalf("expired %v, want only the 8-day-old one", expired)
	}
	for _, id := range []domain.WorkloadID{"recent", "running", "queued", "attempt", "service", "no-time"} {
		if _, ok := wr.Get(id); !ok {
			t.Fatalf("%s was dropped", id)
		}
	}
	if got := wr.expireFinished(now.Add(6*24*time.Hour), finishedRetention, 10, 5); len(got) != 0 {
		t.Fatalf("six days later: expired %v (no-time's age counts from when it was first seen)", got)
	}
	if got := wr.expireFinished(now.Add(8*24*time.Hour), finishedRetention, 10, 5); len(got) != 2 {
		t.Fatalf("eight days later: expired %v, want recent and no-time", got)
	}

	// Count caps: the newest are kept; chats have their own, smaller cap.
	wr = NewWorkloadRegistry()
	for i := 0; i < 6; i++ {
		finishedWorkload(wr, fmt.Sprintf("task-%d", i), "file.hash", domain.WorkloadCompleted, now.Add(-time.Duration(i)*time.Minute))
		finishedWorkload(wr, fmt.Sprintf("chat-%d", i), capLLMChat, domain.WorkloadCompleted, now.Add(-time.Duration(i)*time.Minute-time.Second))
	}
	expired = wr.expireFinished(now, finishedRetention, 8, 3)
	gone := map[domain.WorkloadID]bool{}
	for _, id := range expired {
		gone[id] = true
	}
	// Newest first: task-0 chat-0 task-1 chat-1 task-2 chat-2 task-3 [chat-3 over the chat cap] task-4 [8 kept] ...
	for _, id := range []domain.WorkloadID{"chat-3", "chat-4", "chat-5", "task-5"} {
		if !gone[id] {
			t.Errorf("%s kept; expired %v", id, expired)
		}
	}
	if len(expired) != 4 {
		t.Fatalf("expired %v, want 4", expired)
	}
}

func TestExpiredWorkloadsLeaveTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	store, err := persistent.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, store, Config{})
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, id := range []string{"a", "b"} {
		at := time.Now()
		if id == "a" {
			at = old
		}
		w := domain.Workload{ID: domain.WorkloadID(id)}
		st := domain.WorkloadStatus{ID: w.ID, State: domain.WorkloadCompleted, FinishedAt: at}
		s.persistWorkloadRecord(s.Workloads.Put(w, st))
	}
	s.expireWorkloads()
	store.Close()
	store, err = persistent.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	left, _ := store.ListWorkloads()
	if len(left) != 1 || left[0].Workload.ID != "b" {
		t.Fatalf("after expiry and a reopen: %+v", left)
	}
}
