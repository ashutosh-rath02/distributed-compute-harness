package integration

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/transport/ws"
)

func echoArgs(args ...string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", append([]string{"/C", "echo"}, args...)
	}
	return "echo", args
}

func sleepArgs(seconds string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds " + seconds}
	}
	return "sleep", []string{seconds}
}

func failArgs() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", "exit 7"}
	}
	return "sh", []string{"-c", "exit 7"}
}

func TestWorkloadRunsToCompletion(t *testing.T) {
	const addr = "127.0.0.1:19260"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "workload-agent-a")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := echoArgs("hello-workload")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && (rec.Status.State == domain.WorkloadCompleted || rec.Status.State == domain.WorkloadFailed)
	})

	rec, _ := srv.Workloads.Get(wl.ID)
	if rec.Status.State != domain.WorkloadCompleted {
		t.Fatalf("expected COMPLETED, got %s (error: %s)", rec.Status.State, rec.Status.Error)
	}
	if rec.Status.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", rec.Status.ExitCode)
	}
}

func TestWorkloadAutoPicksReadyNode(t *testing.T) {
	const addr = "127.0.0.1:19261"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "workload-agent-b")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := echoArgs("auto")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Empty target: v1's entire "scheduling" is "explicit target, or the
	// first READY node" — no resource-fit logic.
	wl, err := srv.SubmitWorkload(ctx, "", cmd, args)
	if err != nil {
		t.Fatalf("SubmitWorkload with empty target: %v", err)
	}
	if wl.Target != a.NodeID() {
		t.Fatalf("expected auto-picked target %s, got %s", a.NodeID(), wl.Target)
	}
}

func TestWorkloadNoReadyNodeFails(t *testing.T) {
	const addr = "127.0.0.1:19262"
	srv := startManager(t, addr, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd, args := echoArgs("nobody-home")
	if _, err := srv.SubmitWorkload(ctx, "", cmd, args); err == nil {
		t.Fatal("expected SubmitWorkload to fail when no node is READY")
	}
}

func TestWorkloadCapturesNonZeroExit(t *testing.T) {
	const addr = "127.0.0.1:19263"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "workload-agent-c")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := failArgs()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadFailed
	})
	rec, _ := srv.Workloads.Get(wl.ID)
	if rec.Status.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", rec.Status.ExitCode)
	}
}

func TestWorkloadCancel(t *testing.T) {
	const addr = "127.0.0.1:19264"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "workload-agent-d")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	cmd, args := sleepArgs("30")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), cmd, args)
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
}

// TestWorkloadFailsWhenNodeDisconnectsMidRun proves a workload doesn't
// stay RUNNING forever if its node goes offline before reporting a
// terminal status — the workload analogue of the pending-command-leak fix.
func TestWorkloadFailsWhenNodeDisconnectsMidRun(t *testing.T) {
	const addr = "127.0.0.1:19265"
	srv := startManager(t, addr, 30*time.Second)

	nodeID, conn := registerRawSilentNode(t, addr, "flaky-workload-agent")
	defer conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, nodeID, "sleep", []string{"30"})
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	// The raw node never replies with a RUNNING/terminal status — it stays
	// PENDING from the manager's point of view, exactly like a real
	// workload could still be PENDING when the connection dies.
	conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadFailed
	})
}

// TestWorkloadDisconnectFreesNodeForNewWorkload proves the agent actually
// kills an in-flight workload when its connection to the manager drops
// (Executor.CancelCurrent, called from agent.Run's reconnect loop) —
// without it, the manager's unilateral FAILED verdict from
// failWorkloadsFor would be a lie, and the agent's "only one workload at a
// time" slot would stay wedged even after the node reconnects.
func TestWorkloadDisconnectFreesNodeForNewWorkload(t *testing.T) {
	const addr = "127.0.0.1:19267"
	srv := startManager(t, addr, 30*time.Second)

	ctx0, cancel0 := context.WithCancel(context.Background())
	t.Cleanup(cancel0)
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "workload-agent-f"),
		Name:              "workload-agent-f",
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx0)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	sleepCmd, sleepArgv := sleepArgs("30")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv)
	if err != nil {
		t.Fatalf("SubmitWorkload (first): %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(first.ID)
		return ok && rec.Status.State == domain.WorkloadRunning
	})

	// Simulate the connection dropping without the agent process exiting:
	// find the agent's live connection via the registry and close it
	// directly, exactly as a real network interruption would.
	rec, ok := srv.Registry.Get(a.NodeID())
	if !ok || rec.Conn == nil {
		t.Fatal("expected a live connection to close")
	}
	rec.Conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(first.ID)
		return ok && rec.Status.State == domain.WorkloadFailed
	})

	// The agent should reconnect (same identity) and, having killed the
	// first workload locally, accept a brand new one immediately.
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	echoCmd, echoArgv := echoArgs("after-reconnect")
	second, err := srv.SubmitWorkload(ctx, a.NodeID(), echoCmd, echoArgv)
	if err != nil {
		t.Fatalf("SubmitWorkload (second, after reconnect): %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(second.ID)
		return ok && rec.Status.State == domain.WorkloadCompleted
	})
}

func TestWorkloadRejectsSecondConcurrentAssignOnSameNode(t *testing.T) {
	const addr = "127.0.0.1:19266"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "workload-agent-e")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	sleepCmd, sleepArgv := sleepArgs("5")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv)
	if err != nil {
		t.Fatalf("SubmitWorkload (first): %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(first.ID)
		return ok && rec.Status.State == domain.WorkloadRunning
	})

	echoCmd, echoArgv := echoArgs("second")
	second, err := srv.SubmitWorkload(ctx, a.NodeID(), echoCmd, echoArgv)
	if err != nil {
		t.Fatalf("SubmitWorkload (second): %v", err)
	}

	// The agent rejects the second assignment (only one workload at a time
	// per node in v0) — the manager should see that as a reported FAILED
	// status, not silence.
	waitFor(t, 2*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(second.ID)
		return ok && rec.Status.State == domain.WorkloadFailed
	})

	if err := srv.CancelWorkload(ctx, first.ID); err != nil {
		t.Fatalf("CancelWorkload cleanup: %v", err)
	}
}
