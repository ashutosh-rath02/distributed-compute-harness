package instancelock

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// The lock must hold across processes (the real duplicate case), so the
// contender runs as a separate process: this test binary re-executed.
func TestLockIsExclusiveAcrossProcessesAndFreedOnExit(t *testing.T) {
	if dir := os.Getenv("INSTANCELOCK_CHILD_DIR"); dir != "" {
		_, err := TryAcquire(dir)
		if errors.Is(err, ErrHeld) {
			os.Exit(3)
		}
		if err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	lock, err := TryAcquire(dir)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	child := func() int {
		cmd := exec.Command(os.Args[0], "-test.run=TestLockIsExclusiveAcrossProcessesAndFreedOnExit")
		cmd.Env = append(os.Environ(), "INSTANCELOCK_CHILD_DIR="+dir)
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("child: %v", err)
		}
		return 0
	}
	if code := child(); code != 3 {
		t.Fatalf("a second process acquired a held lock (exit %d, want 3)", code)
	}
	lock.Release()
	time.Sleep(50 * time.Millisecond)
	if code := child(); code != 0 {
		t.Fatalf("lock not acquirable after release (exit %d)", code)
	}
}
