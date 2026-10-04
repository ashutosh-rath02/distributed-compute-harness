package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
)

// Queue priorities (manager queue.go).

type priorityView struct {
	State     domain.WorkloadState `json:"state"`
	Priority  domain.Priority      `json:"priority"`
	StartedAt time.Time            `json:"startedAt"`
}

// With the only slot busy, waiting work starts by priority — high, then
// normal, then low oldest first — and the API takes and shows it.
func TestHighPriorityWorkJumpsTheQueue(t *testing.T) {
	const addr = "127.0.0.1:19653"
	srv, api := startManagerWithAPI(t, addr, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startAgentWithSlots(t, ctx, addr, "prio-agent", 1)
	waitReady(t, srv, a.NodeID())

	cmd, args := sleepArgs("3")
	code, out := postWorkload(t, api.URL, map[string]any{"command": cmd, "args": args})
	if code != http.StatusAccepted {
		t.Fatalf("blocker: %d %v", code, out)
	}
	blocker := out["id"].(string)
	waitFor(t, 10*time.Second, func() bool { return workloadState(srv, domain.WorkloadID(blocker)) == domain.WorkloadRunning })

	if code, out := postWorkload(t, api.URL, map[string]any{"capability": "system.identity", "priority": "urgent"}); code != http.StatusBadRequest {
		t.Fatalf("an unknown priority must be refused: %d %v", code, out)
	}
	submit := func(priority string) string {
		t.Helper()
		// A second each (one thread): start times a clock tick apart.
		body := map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "1", "threads": "1"}}
		if priority != "" {
			body["priority"] = priority
		}
		code, out := postWorkload(t, api.URL, body)
		if code != http.StatusAccepted || out["state"] != string(domain.WorkloadQueued) {
			t.Fatalf("POST (%s): %d %v, want 202 QUEUED", priority, code, out)
		}
		return out["id"].(string)
	}
	lowOld := submit("low")
	normal := submit("") // the default
	lowNew := submit("low")
	high := submit("high")

	var view priorityView
	getJSON(t, api.URL+"/workloads/"+normal, &view)
	if view.Priority != domain.PriorityNormal {
		t.Fatalf("GET shows priority %q for a default submission, want normal", view.Priority)
	}
	var list []priorityView
	getJSON(t, api.URL+"/workloads", &list)
	for _, w := range list {
		if w.Priority == "" {
			t.Fatal("the workload list must show every workload's priority")
		}
	}

	order := []string{high, normal, lowOld, lowNew}
	started := make([]time.Time, len(order))
	waitFor(t, 60*time.Second, func() bool {
		for i, id := range order {
			getJSON(t, api.URL+"/workloads/"+id, &view)
			if view.State != domain.WorkloadCompleted {
				return false
			}
			started[i] = view.StartedAt
		}
		return true
	})
	for i := 1; i < len(order); i++ {
		if !started[i-1].Before(started[i]) {
			t.Fatalf("start order wrong at %d: %v (want high, normal, low, newer low)", i, started)
		}
	}
}

// A job's next attempt (here a low-priority retry) must not take a slot
// that just freed while higher-priority work is waiting for one: the
// queue gets the slot first.
func TestLowJobRetryDoesNotTakeTheSlotFromQueuedHighWork(t *testing.T) {
	const addr = "127.0.0.1:19654"
	srv := startManagerWithReconcile(t, addr, 30*time.Second, time.Hour) // only kicks drive the queue
	nodeID, closer := registerRawNodeWithManifest(t, addr, "retry-node", func(m *domain.Manifest) {
		m.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
		m.WorkloadSlots = 1
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	waitReady(t, srv, nodeID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	assigns := make(chan domain.Workload, 16)
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
				assigns <- p.Workload
			}
		}
	}()
	next := func() domain.Workload {
		t.Helper()
		select {
		case w := <-assigns:
			return w
		case <-time.After(10 * time.Second):
			t.Fatal("no assignment")
			return domain.Workload{}
		}
	}
	report := func(id domain.WorkloadID, state domain.WorkloadState) {
		t.Helper()
		now := time.Now().UTC()
		sendRaw(t, conn, nodeID, protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{
			ID: id, Target: nodeID, State: state, StartedAt: now, FinishedAt: now, Error: map[bool]string{true: "exit 1"}[state == domain.WorkloadFailed]}})
	}

	job, err := srv.SubmitJob(ctx, manager.JobSpec{Tasks: []domain.TaskSpec{{Command: "x"}}, MaxAttempts: 2, Priority: domain.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	first := next()
	if first.Job != job.ID || first.Priority != domain.PriorityLow {
		t.Fatalf("expected the low job's first attempt, got %+v", first)
	}
	report(first.ID, domain.WorkloadRunning)
	high, err := srv.Submit(ctx, manager.WorkloadSpec{Command: "y", Priority: domain.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}
	if got := workloadState(srv, high.ID); got != domain.WorkloadQueued {
		t.Fatalf("high work with the only slot busy: %s, want QUEUED", got)
	}

	report(first.ID, domain.WorkloadFailed) // the slot frees; the task is due a retry
	if got := next(); got.ID != high.ID {
		t.Fatalf("the freed slot went to %s (job %q attempt %d), not the waiting high-priority work", got.ID, got.Job, got.Attempt)
	}
	report(high.ID, domain.WorkloadCompleted)
	if retry := next(); retry.Job != job.ID || retry.Attempt != 2 || retry.Priority != domain.PriorityLow {
		t.Fatalf("expected the job's low-priority retry next, got %+v", retry)
	}
}
