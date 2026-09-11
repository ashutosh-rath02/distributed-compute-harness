package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func readWorkload(id domain.WorkloadID, path string) domain.Workload {
	return domain.Workload{ID: id, Capability: domain.CapabilityFilesystemRead, Params: map[string]string{"path": path}}
}

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

func TestExecutorFilesystemReadReturnsContent(t *testing.T) {
	e := NewExecutor()
	path := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(path, []byte("hello from disk"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	statuses := collectStatuses(t, e, readWorkload("fr1", path))
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
	if final.Stdout != "hello from disk" {
		t.Fatalf("expected file content in Stdout, got %q", final.Stdout)
	}
	if final.Truncated {
		t.Fatal("expected Truncated false for a small file")
	}
	if final.StartedAt.IsZero() {
		t.Fatal("expected StartedAt to be set for a real invocation")
	}
}

func TestExecutorFilesystemReadMissingPathFails(t *testing.T) {
	e := NewExecutor()
	statuses := collectStatuses(t, e, readWorkload("fr2", ""))
	final := statuses[len(statuses)-1]
	if final.State != domain.WorkloadFailed {
		t.Fatalf("expected FAILED for a missing path param, got %s", final.State)
	}
	if final.StartedAt.IsZero() {
		t.Fatal("expected StartedAt to be set — this is a real invocation failure, not a placement rejection")
	}
}

func TestExecutorFilesystemReadNonexistentFileFails(t *testing.T) {
	e := NewExecutor()
	path := filepath.Join(t.TempDir(), "does-not-exist.txt")
	statuses := collectStatuses(t, e, readWorkload("fr3", path))
	final := statuses[len(statuses)-1]
	if final.State != domain.WorkloadFailed {
		t.Fatalf("expected FAILED for a nonexistent file, got %s", final.State)
	}
	if final.StartedAt.IsZero() {
		t.Fatal("expected StartedAt to be set — this is a real invocation failure, not a placement rejection")
	}
}

func TestExecutorFilesystemReadTruncatesOversizedFile(t *testing.T) {
	e := NewExecutor()
	path := filepath.Join(t.TempDir(), "big.txt")
	content := strings.Repeat("A", domain.OutputCapBytes+1024)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	statuses := collectStatuses(t, e, readWorkload("fr4", path))
	final := statuses[len(statuses)-1]
	if final.State != domain.WorkloadCompleted {
		t.Fatalf("expected COMPLETED for an oversized (but readable) file, got %s (error: %s)", final.State, final.Error)
	}
	if !final.Truncated {
		t.Fatal("expected Truncated true for a file larger than OutputCapBytes")
	}
	if len(final.Stdout) != domain.OutputCapBytes {
		t.Fatalf("expected content capped at %d bytes, got %d", domain.OutputCapBytes, len(final.Stdout))
	}
}

// TestExecutorRejectsConcurrentCapabilityInvocation proves the single-slot
// invariant is capability-agnostic: a filesystem.read can't start while a
// system.execute is running, and vice versa — not just two of the same
// kind, which TestExecutorRejectsConcurrentWorkload already covers.
func TestExecutorRejectsConcurrentCapabilityInvocation(t *testing.T) {
	e := NewExecutor()
	done := make(chan struct{})
	err := e.Start(context.Background(), sleepWorkload("cw1", "2"), func(s domain.WorkloadStatus) {
		if s.State != domain.WorkloadRunning {
			close(done)
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err = e.Start(context.Background(), readWorkload("cw2", path), func(domain.WorkloadStatus) {})
	if err == nil {
		t.Fatal("expected a filesystem.read to be rejected while a system.execute is running")
	}

	if cancelErr := e.Cancel("cw1"); cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled workload to finish")
	}
}

// TestExecutorCancelFilesystemReadFreesSlot proves canceling a
// filesystem.read leaves the executor immediately reusable, even though
// the underlying read (on a real, fast filesystem) most likely already
// finished by the time Cancel is called — either outcome (COMPLETED or
// CANCELED) is acceptable here, since both mean the slot was freed
// promptly, which is the property that actually matters (see
// startFilesystemRead's doc comment on why a hang can't be simulated
// portably/deterministically in this test).
func TestExecutorCancelFilesystemReadFreesSlot(t *testing.T) {
	e := NewExecutor()
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("A", 4*1024*1024)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var final domain.WorkloadStatus
	done := make(chan struct{})
	err := e.Start(context.Background(), readWorkload("fr5", path), func(s domain.WorkloadStatus) {
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
	_ = e.Cancel("fr5") // may legitimately fail if the read already finished — that's fine

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a terminal status")
	}

	mu.Lock()
	state := final.State
	mu.Unlock()
	if state != domain.WorkloadCompleted && state != domain.WorkloadCanceled {
		t.Fatalf("expected COMPLETED or CANCELED, got %s", state)
	}

	// The real assertion: the executor is immediately usable again.
	statuses := collectStatuses(t, e, echoWorkload("fr6", "still-works"))
	if final := statuses[len(statuses)-1]; final.State != domain.WorkloadCompleted {
		t.Fatalf("expected executor to accept a new workload right after, got %s", final.State)
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
