package integration

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// TestManagerRejectsInvalidWorkloadStateFromAgent: a hostile (or buggy)
// enrolled agent controls the state string in its WORKLOAD_STATUS. The
// dashboard renders states into markup and holds the operator credential,
// so the manager must only accept states an execution can really report.
func TestManagerRejectsInvalidWorkloadStateFromAgent(t *testing.T) {
	const addr = "127.0.0.1:19507"
	srv := startManager(t, addr, 2*time.Second)
	nodeID, conn := registerRawSilentNode(t, addr, "hostile-agent")
	defer conn.Close()
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, nodeID, "cmd", []string{"/C", "echo", "x"}, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}
	report := func(state domain.WorkloadState) {
		t.Helper()
		env, err := protocol.NewEnvelope(protocol.MsgWorkloadStatus, nodeID, domain.ManagerNodeID,
			protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{ID: wl.ID, Target: nodeID, State: state}})
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := protocol.Encode(env)
		if err := conn.Send(ctx, wire); err != nil {
			t.Fatalf("send WORKLOAD_STATUS: %v", err)
		}
	}

	for _, hostile := range []domain.WorkloadState{`x" onmouseover="alert(1)`, "<img src=x onerror=alert(1)>", domain.WorkloadPending, domain.WorkloadUnknown} {
		report(hostile)
	}
	report(domain.WorkloadRunning) // a legitimate report still lands, in order
	waitFor(t, 3*time.Second, func() bool {
		rec, _ := srv.Workloads.Get(wl.ID)
		return rec.Status.State == domain.WorkloadRunning
	})
	report(`COMPLETED" class="x`)
	time.Sleep(200 * time.Millisecond)
	if rec, _ := srv.Workloads.Get(wl.ID); rec.Status.State != domain.WorkloadRunning {
		t.Fatalf("an invalid state from the agent was accepted: %q", rec.Status.State)
	}
}
