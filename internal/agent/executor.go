package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// Executor runs at most one workload at a time on this node (v1 keeps
// concurrency simple and explicit: a second ASSIGN while one is already
// running is rejected rather than silently queued or run in parallel).
type Executor struct {
	mu      sync.Mutex
	current *runningWorkload
	// canceledBeforeStart records IDs that were canceled before Start was
	// ever called for them. Ordinarily WORKLOAD_ASSIGN and WORKLOAD_CANCEL
	// arrive strictly in order on the same connection (the agent's receive
	// loop is sequential), so this should never be consulted — it exists
	// as a safety net rather than a mechanism the normal path relies on.
	canceledBeforeStart map[domain.WorkloadID]bool
}

type runningWorkload struct {
	id       domain.WorkloadID
	cancel   context.CancelFunc
	canceled bool
}

// NewExecutor returns an idle Executor.
func NewExecutor() *Executor {
	return &Executor{canceledBeforeStart: make(map[domain.WorkloadID]bool)}
}

// Start begins running wl as a subprocess, invoking onStatus once
// immediately with WorkloadRunning and once more with the terminal status
// (COMPLETED/FAILED/CANCELED) when it finishes. onStatus is called from a
// background goroutine for the terminal update, never after Start returns
// for the initial one.
//
// Command/Args are argv-form and passed directly to exec.Command — never
// through a shell — so there is no injection surface and no
// cmd.exe-vs-sh ambiguity about how a string would be split.
//
// Cancellation (via Cancel) kills only the direct child process. A
// workload that spawns its own children can leak them; v1 does not
// implement process-group/job-object handling to prevent that.
func (e *Executor) Start(ctx context.Context, wl domain.Workload, onStatus func(domain.WorkloadStatus)) error {
	e.mu.Lock()
	if e.canceledBeforeStart[wl.ID] {
		delete(e.canceledBeforeStart, wl.ID)
		e.mu.Unlock()
		onStatus(domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadCanceled, Error: "canceled before it started running",
		})
		return nil
	}
	if e.current != nil {
		busy := e.current.id
		e.mu.Unlock()
		return fmt.Errorf("executor: workload %s is already running; only one workload per node at a time in v0", busy)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, wl.Command, wl.Args...)
	stdout := &cappedBuffer{limit: domain.OutputCapBytes}
	stderr := &cappedBuffer{limit: domain.OutputCapBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	rw := &runningWorkload{id: wl.ID, cancel: cancel}
	e.current = rw
	e.mu.Unlock()

	startedAt := time.Now().UTC()

	if err := cmd.Start(); err != nil {
		cancel()
		e.clear(wl.ID)
		onStatus(domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed,
			Error: err.Error(), StartedAt: startedAt, FinishedAt: time.Now().UTC(),
		})
		return nil
	}

	onStatus(domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, State: domain.WorkloadRunning, StartedAt: startedAt})

	go func() {
		waitErr := cmd.Wait()
		defer cancel()
		canceled := e.clear(wl.ID)
		finishedAt := time.Now().UTC()

		status := domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target,
			Stdout: stdout.String(), Stderr: stderr.String(),
			Truncated: stdout.truncated || stderr.truncated,
			StartedAt: startedAt, FinishedAt: finishedAt,
		}

		switch {
		case canceled:
			status.State = domain.WorkloadCanceled
			status.Error = "canceled"
		case waitErr != nil:
			status.State = domain.WorkloadFailed
			status.Error = waitErr.Error()
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				status.ExitCode = exitErr.ExitCode()
			}
		default:
			status.State = domain.WorkloadCompleted
		}

		onStatus(status)
	}()

	return nil
}

// Cancel terminates the running workload with the given ID. It returns an
// error if no workload with that ID is currently running (already finished,
// wrong ID, or nothing running) — cancellation of a workload that already
// completed is not an error the caller can act on, but is reported as one
// so a stale WORKLOAD_CANCEL doesn't look like it silently succeeded.
func (e *Executor) Cancel(id domain.WorkloadID) error {
	e.mu.Lock()
	rw := e.current
	if rw == nil || rw.id != id {
		e.canceledBeforeStart[id] = true
		e.mu.Unlock()
		return fmt.Errorf("executor: no running workload with id %s (noted in case its ASSIGN hasn't arrived yet)", id)
	}
	rw.canceled = true
	e.mu.Unlock()

	rw.cancel()
	return nil
}

// CancelCurrent terminates whatever workload is currently running,
// regardless of ID, and is a no-op if none is. It exists for connection
// loss: the manager marks a node's in-flight workloads FAILED as soon as
// it goes offline (failWorkloadsFor), so leaving the agent to keep running
// one afterward would leave the two sides permanently disagreeing about
// whether it's still going.
func (e *Executor) CancelCurrent() {
	e.mu.Lock()
	rw := e.current
	if rw != nil {
		rw.canceled = true
	}
	e.mu.Unlock()
	if rw != nil {
		rw.cancel()
	}
}

// clear removes id as the current workload if it still is one, and reports
// whether it had been marked canceled. It is the single place that reads
// and resets "current", so Start's completion goroutine and Cancel never
// race over whether a workload was canceled vs. finished naturally at
// nearly the same moment.
func (e *Executor) clear(id domain.WorkloadID) (canceled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil && e.current.id == id {
		canceled = e.current.canceled
		e.current = nil
	}
	return canceled
}

// cappedBuffer accumulates written bytes up to limit, discarding anything
// beyond it (and recording that it did) rather than growing unbounded. It
// always reports success to the writer (a subprocess's stdout/stderr pipe)
// so a chatty process is never blocked or errored by the cap.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
