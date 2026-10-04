package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/tasks"
)

// ErrExecutorFull is returned by Start when every workload slot is busy.
// The agent reports it as a retryable refusal, so the manager re-queues
// the workload instead of failing it.
var ErrExecutorFull = errors.New("executor: all workload slots are busy")

// Executor runs up to slots workloads at once on this node (advertised in
// the manifest so the manager reserves capacity accordingly). An ASSIGN
// beyond that is refused, never silently queued or oversubscribed here —
// queueing is the manager's job.
type Executor struct {
	mu      sync.Mutex
	slots   int
	running map[domain.WorkloadID]*runningWorkload
	// canceledBeforeStart records IDs that were canceled before Start was
	// ever called for them. Ordinarily WORKLOAD_ASSIGN and WORKLOAD_CANCEL
	// arrive strictly in order on the same connection (the agent's receive
	// loop is sequential), so this should never be consulted — it exists
	// as a safety net rather than a mechanism the normal path relies on.
	canceledBeforeStart map[domain.WorkloadID]bool
	// workRoot holds one working directory per running workload that
	// declares files (files.go). Empty disables file workloads.
	workRoot string
	// disabled are capabilities the device owner turned off
	// (cmd/agent's -disable-capabilities): never advertised, and refused
	// if assigned anyway.
	disabled map[domain.CapabilityName]bool
	// handlers implement the catalog types (internal/tasks); per agent,
	// since some depend on the device's own configuration (Ollama).
	handlers *tasks.Registry
	// progressEvery is how often a streaming task reports its output so
	// far: often enough that a chat answer reads as it is written. Each
	// report carries the output so far (capped at 64 KiB).
	progressEvery time.Duration
}

type runningWorkload struct {
	id       domain.WorkloadID
	cancel   context.CancelFunc
	canceled bool
}

// NewExecutor returns an idle single-slot Executor.
func NewExecutor() *Executor { return NewExecutorWithSlots(1) }

// NewExecutorWithSlots returns an idle Executor running up to slots
// workloads at once (minimum 1).
func NewExecutorWithSlots(slots int) *Executor {
	if slots < 1 {
		slots = 1
	}
	return &Executor{slots: slots, running: make(map[domain.WorkloadID]*runningWorkload), canceledBeforeStart: make(map[domain.WorkloadID]bool),
		handlers: tasks.Builtins(), progressEvery: 300 * time.Millisecond}
}

// Slots reports how many workloads this executor runs at once.
func (e *Executor) Slots() int { return e.slots }

// Running reports how many workloads are running now.
func (e *Executor) Running() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.running)
}

// Start begins running wl, invoking onStatus once immediately with
// WorkloadRunning and once more with the terminal status
// (COMPLETED/FAILED/CANCELED) when it finishes. onStatus is called from a
// background goroutine for the terminal update, never after Start returns
// for the initial one. What "running" means depends on wl's capability
// (EffectiveCapability) — see startExecute/startFilesystemRead.
//
// The busy/canceled-before-start checks and slot registration below are
// capability-agnostic: every workload, whatever it invokes, takes one of
// the executor's slots.
func (e *Executor) Start(ctx context.Context, wl domain.Workload, onStatus func(domain.WorkloadStatus)) error {
	return e.StartWithFiles(ctx, wl, nil, onStatus)
}

// StartWithFiles is Start for an assignment that may declare input/output
// files, moved with xfer (files.go). For a workload without files it is
// exactly Start.
func (e *Executor) StartWithFiles(ctx context.Context, wl domain.Workload, xfer ArtifactTransfer, onStatus func(domain.WorkloadStatus)) error {
	capability := wl.EffectiveCapability()
	refuse := func(msg string) error {
		onStatus(domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed, Error: msg})
		return nil
	}
	// Everything that needs no executor state is checked first, outside
	// the lock: a handler's availability probe may be an HTTP call.
	if e.disabled[capability] {
		return refuse(fmt.Sprintf("capability %q is disabled on this device", capability))
	}
	var handler tasks.Handler
	var streams bool
	if t, ok := catalog.Lookup(capability); ok {
		h, ok := e.handlers.Lookup(capability)
		if !ok {
			return refuse(fmt.Sprintf("this agent has no handler for %s", capability))
		}
		if err := h.Available(ctx); err != nil {
			return refuse(fmt.Sprintf("%s is unavailable on this device: %v", capability, err))
		}
		// The agent links the same catalog: re-validate what the manager
		// compiled, so a version mismatch is refused rather than misread.
		params, outputs, err := t.Compile(wl.Params, wl.Inputs)
		if err != nil {
			return refuse(fmt.Sprintf("assignment doesn't match this agent's %s v%s: %v", capability, t.Version, err))
		}
		if !sameParams(params, wl.Params) || !slices.Equal(outputs, wl.Outputs) {
			return refuse(fmt.Sprintf("assignment doesn't match this agent's %s v%s (update the agent or the manager)", capability, t.Version))
		}
		handler, streams = h, t.Streams
	} else if capability != domain.CapabilitySystemExecute && capability != domain.CapabilityFilesystemRead {
		// Defense in depth, expected to be unreachable in the normal path:
		// the manager already refuses to dispatch a capability a node
		// hasn't declared (internal/manager/placement.go) — this only
		// fires for a stale manifest or a bypassed check. No StartedAt
		// set, mirroring the insecure-mode-refusal literal in
		// workloads.go: a placement-time rejection, not a real failure, so
		// v3's reconciler defers it rather than counting it against the
		// restart backoff curve.
		return refuse(fmt.Sprintf("agent does not implement capability %q", capability))
	}
	if wl.HasFiles() && capability == domain.CapabilityFilesystemRead {
		return refuse(fmt.Sprintf("capability %q can't take input or output files", capability))
	}

	e.mu.Lock()
	if e.canceledBeforeStart[wl.ID] {
		delete(e.canceledBeforeStart, wl.ID)
		e.mu.Unlock()
		onStatus(domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadCanceled, Error: "canceled before it started running",
		})
		return nil
	}
	if _, dup := e.running[wl.ID]; dup {
		e.mu.Unlock()
		return fmt.Errorf("executor: workload %s is already running here", wl.ID)
	}
	if len(e.running) >= e.slots {
		e.mu.Unlock()
		return fmt.Errorf("%w (%d of %d in use)", ErrExecutorFull, e.slots, e.slots)
	}

	var runCtx context.Context
	var cancel context.CancelFunc
	if wl.TimeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(wl.TimeoutSeconds)*time.Second)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	e.running[wl.ID] = &runningWorkload{id: wl.ID, cancel: cancel}
	e.mu.Unlock()

	switch {
	case handler != nil:
		go e.runTask(runCtx, cancel, wl, xfer, onStatus, handlerRunner(handler, wl), streams)
	case wl.HasFiles():
		go e.runTask(runCtx, cancel, wl, xfer, onStatus, execRunner(wl), false)
	case capability == domain.CapabilitySystemExecute:
		e.startExecute(runCtx, cancel, wl, onStatus)
	case capability == domain.CapabilityFilesystemRead:
		e.startFilesystemRead(runCtx, cancel, wl, onStatus)
	}
	return nil
}

// SetWorkRoot sets where working directories for file workloads go.
func (e *Executor) SetWorkRoot(dir string) { e.workRoot = dir }

// SetHandlers sets the catalog handlers this executor runs.
func (e *Executor) SetHandlers(r *tasks.Registry) { e.handlers = r }

// Handlers returns them (the agent advertises what they offer).
func (e *Executor) Handlers() *tasks.Registry { return e.handlers }

// SetDisabled turns capabilities off on this device.
func (e *Executor) SetDisabled(names []domain.CapabilityName) {
	e.disabled = make(map[domain.CapabilityName]bool, len(names))
	for _, n := range names {
		e.disabled[n] = true
	}
}

// Disabled reports whether the device owner turned name off.
func (e *Executor) Disabled(name domain.CapabilityName) bool { return e.disabled[name] }

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// timedOut rewrites a failure caused by the workload's own deadline.
func timedOut(runCtx context.Context, wl domain.Workload, status *domain.WorkloadStatus) {
	if status.State == domain.WorkloadFailed && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		status.Error = fmt.Sprintf("timed out after %ds", wl.TimeoutSeconds)
	}
}

// resolveCommandPath resolves a bare command name (no path separator) to
// an absolute path ourselves on non-Windows, rather than letting
// exec.Command fall back to its own internal os/exec.LookPath. Confirmed
// via real hardware (an Android/Termux node, v5 phone onboarding): a bare
// command name like "uname" (exactly the kind of thing an operator
// naturally types) reproducibly crashed the *entire agent process*, not
// just the one workload, while the identical command given as an absolute
// path ran fine. The likely mechanism — not independently confirmed beyond
// that observation — is that LookPath's internal syscall.Eaccess
// (faccessat2) is blocked by this device's seccomp policy, killing the
// caller with SIGSYS rather than returning an ordinary error (see
// golang/go#57393, golang/go#60125). Resolving the path ourselves via a
// plain os.Stat-based $PATH search avoids that call regardless of the
// exact mechanism. Left unchanged on Windows, which has no such issue and
// is already verified working via exec.Command's own LookPath.
func resolveCommandPath(name string) string {
	if runtime.GOOS == "windows" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
		return name
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name // not found in PATH; exec.Command will fail with its usual "not found" error
}

// startExecute runs wl as a subprocess — the capability behind v1's
// original workload execution, unchanged, just relocated out of Start so
// it sits alongside its sibling capability handler.
//
// Command/Args are argv-form and passed directly to exec.Command — never
// through a shell — so there is no injection surface and no
// cmd.exe-vs-sh ambiguity about how a string would be split.
//
// Cancellation (via Cancel) kills only the direct child process. A
// workload that spawns its own children can leak them; v1 does not
// implement process-group/job-object handling to prevent that.
func (e *Executor) startExecute(runCtx context.Context, cancel context.CancelFunc, wl domain.Workload, onStatus func(domain.WorkloadStatus)) {
	cmd := exec.CommandContext(runCtx, resolveCommandPath(wl.Command), wl.Args...)
	stdout := &cappedBuffer{limit: domain.OutputCapBytes}
	stderr := &cappedBuffer{limit: domain.OutputCapBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	startedAt := time.Now().UTC()

	if err := cmd.Start(); err != nil {
		cancel()
		e.clear(wl.ID)
		onStatus(domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed,
			Error: err.Error(), StartedAt: startedAt, FinishedAt: time.Now().UTC(),
		})
		return
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
		timedOut(runCtx, wl, &status)
		onStatus(status)
	}()
}

// startFilesystemRead reads wl.Params["path"]'s content, capped at
// domain.OutputCapBytes like startExecute's stdout/stderr already are.
//
// Unlike startExecute (where cancel() kills the real child process and
// unblocks cmd.Wait()), a plain os.Open/Read does not observe ctx — there
// is no way to interrupt a blocked syscall from another goroutine. The
// actual read runs in an inner goroutine; this outer one selects between
// it finishing and runCtx.Done(), so a hang (a slow network mount, a FIFO
// with no writer, ...) can't wedge the executor's single slot forever, at
// the cost of the inner goroutine possibly leaking until the read
// eventually (if ever) unblocks — the same accepted trade-off Cancel's own
// doc comment already makes for a child process that spawns its own
// children.
func (e *Executor) startFilesystemRead(runCtx context.Context, cancel context.CancelFunc, wl domain.Workload, onStatus func(domain.WorkloadStatus)) {
	startedAt := time.Now().UTC()
	onStatus(domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, State: domain.WorkloadRunning, StartedAt: startedAt})

	go func() {
		defer cancel()

		type readResult struct {
			content   string
			truncated bool
			err       error
		}
		done := make(chan readResult, 1)
		go func() {
			content, truncated, err := readFileCapped(wl.Params["path"], domain.OutputCapBytes)
			done <- readResult{content, truncated, err}
		}()

		var res readResult
		var haveResult bool
		select {
		case res = <-done:
			haveResult = true
		case <-runCtx.Done():
		}

		canceled := e.clear(wl.ID)
		finishedAt := time.Now().UTC()
		status := domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, StartedAt: startedAt, FinishedAt: finishedAt}

		switch {
		case canceled:
			status.State = domain.WorkloadCanceled
			status.Error = "canceled"
		case !haveResult:
			// runCtx ended for a reason other than Cancel (e.g. the
			// connection's own context torn down) without the canceled
			// flag being set — report FAILED rather than leaving the
			// manager waiting indefinitely for a status that will never
			// come.
			status.State = domain.WorkloadFailed
			status.Error = "context ended before the read finished"
		case res.err != nil:
			status.State = domain.WorkloadFailed
			status.Error = res.err.Error()
		default:
			status.State = domain.WorkloadCompleted
			status.Stdout = res.content
			status.Truncated = res.truncated
		}
		timedOut(runCtx, wl, &status)
		onStatus(status)
	}()
}

// readFileCapped reads path's content, retaining at most limit bytes
// (mirroring the OutputCapBytes convention startExecute's stdout/stderr
// already use) and reporting whether it was capped, rather than reading an
// arbitrarily large file fully into memory.
func readFileCapped(path string, limit int) (content string, truncated bool, err error) {
	if path == "" {
		return "", false, errors.New(`missing required param "path"`)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(f, int64(limit))); err != nil {
		return "", false, err
	}

	var extra [1]byte
	n, _ := f.Read(extra[:])
	return buf.String(), n > 0, nil
}

// Cancel terminates the running workload with the given ID. It returns an
// error if no workload with that ID is currently running (already finished,
// wrong ID, or nothing running) — cancellation of a workload that already
// completed is not an error the caller can act on, but is reported as one
// so a stale WORKLOAD_CANCEL doesn't look like it silently succeeded.
func (e *Executor) Cancel(id domain.WorkloadID) error {
	e.mu.Lock()
	rw := e.running[id]
	if rw == nil {
		e.canceledBeforeStart[id] = true
		e.mu.Unlock()
		return fmt.Errorf("executor: no running workload with id %s (noted in case its ASSIGN hasn't arrived yet)", id)
	}
	rw.canceled = true
	e.mu.Unlock()

	rw.cancel()
	return nil
}

// CancelCurrent terminates every running workload and is a no-op if none
// is. It exists for connection loss: the manager marks a node's in-flight
// workloads FAILED as soon as it goes offline (failWorkloadsFor), so
// leaving the agent to keep running them afterward would leave the two
// sides permanently disagreeing about whether they're still going.
func (e *Executor) CancelCurrent() {
	e.mu.Lock()
	var all []*runningWorkload
	for _, rw := range e.running {
		rw.canceled = true
		all = append(all, rw)
	}
	e.mu.Unlock()
	for _, rw := range all {
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
	if rw := e.running[id]; rw != nil {
		canceled = rw.canceled
		delete(e.running, id)
	}
	return canceled
}

// cappedBuffer accumulates written bytes up to limit, discarding anything
// beyond it (and recording that it did) rather than growing unbounded. It
// always reports success to the writer (a subprocess's stdout/stderr pipe)
// so a chatty process is never blocked or errored by the cap.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
