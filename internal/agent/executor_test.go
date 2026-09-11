package agent

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func echoWorkload(id domain.WorkloadID, args ...string) domain.Workload {
	if runtime.GOOS == "windows" {
		return domain.Workload{ID: id, Command: "cmd", Args: append([]string{"/C", "echo"}, args...)}
	}
	return domain.Workload{ID: id, Command: "echo", Args: args}
}

func sleepWorkload(id domain.WorkloadID, seconds string) domain.Workload {
	if runtime.GOOS == "windows" {
		return domain.Workload{ID: id, Command: "powershell", Args: []string{"-NoProfile", "-Command", "Start-Sleep -Seconds " + seconds}}
	}
	return domain.Workload{ID: id, Command: "sleep", Args: []string{seconds}}
}

func collectStatuses(t *testing.T, e *Executor, wl domain.Workload) []domain.WorkloadStatus {
	t.Helper()
	var mu sync.Mutex
	var got []domain.WorkloadStatus
	done := make(chan struct{})

	err := e.Start(context.Background(), wl, func(s domain.WorkloadStatus) {
		mu.Lock()
		got = append(got, s)
		terminal := s.State != domain.WorkloadRunning
		mu.Unlock()
		if terminal {
			close(done)
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal workload status")
	}

	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestExecutorRunsToCompletion(t *testing.T) {
	e := NewExecutor()
	statuses := collectStatuses(t, e, echoWorkload("w1", "hello"))

	if len(statuses) != 2 {
		t.Fatalf("expected 2 status updates (running, completed), got %d: %+v", len(statuses), statuses)
	}
	if statuses[0].State != domain.WorkloadRunning {
		t.Fatalf("expected first status RUNNING, got %s", statuses[0].State)
	}
	final := statuses[1]
	if final.State != domain.WorkloadCompleted {
		t.Fatalf("expected COMPLETED, got %s (error: %s)", final.State, final.Error)
	}
	if final.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", final.ExitCode)
	}
}

func TestExecutorCapturesNonZeroExit(t *testing.T) {
	e := NewExecutor()
	var wl domain.Workload
	if runtime.GOOS == "windows" {
		wl = domain.Workload{ID: "w2", Command: "cmd", Args: []string{"/C", "exit 3"}}
	} else {
		wl = domain.Workload{ID: "w2", Command: "sh", Args: []string{"-c", "exit 3"}}
	}

	statuses := collectStatuses(t, e, wl)
	final := statuses[len(statuses)-1]
	if final.State != domain.WorkloadFailed {
		t.Fatalf("expected FAILED for non-zero exit, got %s", final.State)
	}
	if final.ExitCode != 3 {
		t.Fatalf("expected exit code 3, got %d", final.ExitCode)
	}
}

func TestExecutorRejectsConcurrentWorkload(t *testing.T) {
	e := NewExecutor()
	done := make(chan struct{})
	err := e.Start(context.Background(), sleepWorkload("w3", "2"), func(s domain.WorkloadStatus) {
		if s.State != domain.WorkloadRunning {
			close(done)
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = e.Start(context.Background(), echoWorkload("w4", "hi"), func(domain.WorkloadStatus) {})
	if err == nil {
		t.Fatal("expected second concurrent Start to be rejected")
	}

	if cancelErr := e.Cancel("w3"); cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled workload to finish")
	}
}

func TestExecutorCancel(t *testing.T) {
	e := NewExecutor()
	var mu sync.Mutex
	var final domain.WorkloadStatus
	done := make(chan struct{})

	err := e.Start(context.Background(), sleepWorkload("w5", "30"), func(s domain.WorkloadStatus) {
		if s.State == domain.WorkloadRunning {
			return
		}
		mu.Lock()
		final = s
		mu.Unlock()
		close(done)
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := e.Cancel("w5"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancellation to take effect")
	}

	mu.Lock()
	defer mu.Unlock()
	if final.State != domain.WorkloadCanceled {
		t.Fatalf("expected CANCELED, got %s", final.State)
	}
}

func TestExecutorCancelBeforeStartCancelsOnArrival(t *testing.T) {
	e := NewExecutor()
	if err := e.Cancel("w-not-yet-started"); err == nil {
		t.Fatal("expected an error reporting the ID isn't currently running")
	}

	statuses := collectStatuses(t, e, echoWorkload("w-not-yet-started", "should-not-run"))
	if len(statuses) != 1 {
		t.Fatalf("expected exactly 1 status (no RUNNING should be reported), got %d: %+v", len(statuses), statuses)
	}
	if statuses[0].State != domain.WorkloadCanceled {
		t.Fatalf("expected CANCELED for a workload canceled before Start, got %s", statuses[0].State)
	}
}

func TestExecutorCancelCurrentIsNoOpWhenIdle(t *testing.T) {
	e := NewExecutor()
	e.CancelCurrent() // must not panic
}

func TestExecutorCancelCurrentKillsRunningWorkload(t *testing.T) {
	e := NewExecutor()
	var mu sync.Mutex
	var final domain.WorkloadStatus
	done := make(chan struct{})

	err := e.Start(context.Background(), sleepWorkload("w-cancel-current", "30"), func(s domain.WorkloadStatus) {
		if s.State == domain.WorkloadRunning {
			return
		}
		mu.Lock()
		final = s
		mu.Unlock()
		close(done)
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	e.CancelCurrent()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for CancelCurrent to take effect")
	}

	mu.Lock()
	defer mu.Unlock()
	if final.State != domain.WorkloadCanceled {
		t.Fatalf("expected CANCELED, got %s", final.State)
	}
}

func TestExecutorCancelUnknownIDFails(t *testing.T) {
	e := NewExecutor()
	if err := e.Cancel("does-not-exist"); err == nil {
		t.Fatal("expected error canceling a workload that isn't running")
	}
}

func TestExecutorAllowsSequentialWorkloads(t *testing.T) {
	e := NewExecutor()
	collectStatuses(t, e, echoWorkload("w6", "first"))
	statuses := collectStatuses(t, e, echoWorkload("w7", "second"))
	final := statuses[len(statuses)-1]
	if final.State != domain.WorkloadCompleted {
		t.Fatalf("expected second sequential workload to complete, got %s", final.State)
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	orig := domain.OutputCapBytes
	_ = orig // OutputCapBytes is a const; test the cappedBuffer directly instead.

	c := &cappedBuffer{limit: 10}
	n, err := c.Write([]byte("0123456789ABCDEF"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 16 {
		t.Fatalf("expected Write to report all 16 bytes consumed, got %d", n)
	}
	if c.String() != "0123456789" {
		t.Fatalf("expected buffer capped at 10 bytes, got %q", c.String())
	}
	if !c.truncated {
		t.Fatal("expected truncated to be true")
	}
}
