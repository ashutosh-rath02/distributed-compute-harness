package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"home-harness/internal/domain"
)

// ArtifactTransfer moves one assignment's files between the manager and
// this agent (one is made per assignment: it carries that assignment's
// token). Fetch must verify the content against ref before placing it at
// dest; Upload reports what the manager stored.
type ArtifactTransfer interface {
	Fetch(ctx context.Context, ref domain.ArtifactRef, dest string) error
	Upload(ctx context.Context, name, src string) (domain.ArtifactRef, error)
}

// runWithFiles is startExecute for a workload that declares input or
// output files: it gets a fresh working directory (its only cwd), its
// inputs are fetched and verified before the process starts, and its
// declared outputs are uploaded after it exits successfully — all while
// the workload still holds its slot, since the transfers use this
// device's disk and network too. The directory is removed before the
// terminal status is reported.
//
// Output contract (strict, so batch jobs can rely on it): every declared
// output must exist as a regular file after a successful run, or the
// workload FAILS. A failed or canceled run uploads nothing.
func (e *Executor) runWithFiles(runCtx context.Context, cancel context.CancelFunc, wl domain.Workload, xfer ArtifactTransfer, onStatus func(domain.WorkloadStatus)) {
	defer cancel()
	startedAt := time.Now().UTC()
	var dir string
	finish := func(status domain.WorkloadStatus) {
		if dir != "" {
			os.RemoveAll(dir) // before the report: "finished" means cleaned up
		}
		status.ID, status.Target, status.StartedAt, status.FinishedAt = wl.ID, wl.Target, startedAt, time.Now().UTC()
		if e.clear(wl.ID) {
			status.State, status.Error, status.Outputs = domain.WorkloadCanceled, "canceled", nil
		}
		onStatus(status)
	}
	fail := func(err error) { finish(domain.WorkloadStatus{State: domain.WorkloadFailed, Error: err.Error()}) }

	if xfer == nil || e.workRoot == "" {
		fail(errors.New("this agent can't transfer workload files (no transfer channel or work directory)"))
		return
	}
	if !domain.ValidWorkloadID(wl.ID) {
		fail(fmt.Errorf("workload ID %q can't name a working directory", wl.ID))
		return
	}
	if err := domain.ValidateWorkloadFiles(wl.Inputs, wl.Outputs); err != nil {
		fail(err)
		return
	}
	dir = filepath.Join(e.workRoot, string(wl.ID))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(fmt.Errorf("create working directory: %w", err))
		return
	}

	for _, in := range wl.Inputs {
		dest := filepath.Join(dir, filepath.FromSlash(in.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			fail(fmt.Errorf("input %s: %w", in.Name, err))
			return
		}
		if err := xfer.Fetch(runCtx, in, dest); err != nil {
			fail(fmt.Errorf("fetch input %s: %w", in.Name, err))
			return
		}
	}
	// Outputs' parent directories exist up front, so a program can write
	// "out/result.txt" without creating "out" itself.
	for _, name := range wl.Outputs {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(name))), 0o700); err != nil {
			fail(fmt.Errorf("output %s: %w", name, err))
			return
		}
	}

	cmd := exec.CommandContext(runCtx, resolveCommandPath(wl.Command), wl.Args...)
	cmd.Dir = dir
	stdout := &cappedBuffer{limit: domain.OutputCapBytes}
	stderr := &cappedBuffer{limit: domain.OutputCapBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}
	onStatus(domain.WorkloadStatus{ID: wl.ID, Target: wl.Target, State: domain.WorkloadRunning, StartedAt: startedAt})
	waitErr := cmd.Wait()

	status := domain.WorkloadStatus{
		Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated,
	}
	if waitErr != nil {
		status.State, status.Error = domain.WorkloadFailed, waitErr.Error()
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			status.ExitCode = exitErr.ExitCode()
		}
		finish(status)
		return
	}
	if e.isCanceled(wl.ID) {
		finish(status)
		return
	}
	for _, name := range wl.Outputs {
		path := filepath.Join(dir, filepath.FromSlash(name))
		fi, err := os.Lstat(path)
		switch {
		case err != nil:
			status.State, status.Error = domain.WorkloadFailed, fmt.Sprintf("declared output %s was not produced", name)
		case !fi.Mode().IsRegular():
			status.State, status.Error = domain.WorkloadFailed, fmt.Sprintf("declared output %s is not a regular file", name)
		}
		if status.State == domain.WorkloadFailed {
			status.Outputs = nil
			finish(status)
			return
		}
		ref, err := xfer.Upload(runCtx, name, path)
		if err != nil {
			status.State, status.Error, status.Outputs = domain.WorkloadFailed, fmt.Sprintf("upload output %s: %v", name, err), nil
			finish(status)
			return
		}
		status.Outputs = append(status.Outputs, ref)
	}
	status.State = domain.WorkloadCompleted
	finish(status)
}

// isCanceled reports whether id has been canceled, without releasing its
// slot.
func (e *Executor) isCanceled(id domain.WorkloadID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rw := e.running[id]
	return rw != nil && rw.canceled
}

// cleanWorkRoot removes working directories a crashed or killed run left
// behind. Only entries named like a workload ID are touched.
func cleanWorkRoot(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if domain.ValidWorkloadID(domain.WorkloadID(ent.Name())) {
			os.RemoveAll(filepath.Join(root, ent.Name()))
		}
	}
}
