package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/tasks"
)

// Defaults for Options.
const (
	// DefaultMaxOutBytes bounds what one container may write to /out: it
	// is a folder on the device's own disk, with no quota of its own.
	DefaultMaxOutBytes = 1 << 30
	// maxOutFiles bounds the files and folders in /out.
	maxOutFiles     = 10000
	defaultProbeTTL = 15 * time.Second
)

// The staging folders inside a workload's working directory: inputs are
// moved into inDir (mounted at /in) and outputs copied back from outDir
// (/out). '~' never appears in a workload file name, so neither can
// collide with one.
const (
	inDirName  = "~in"
	outDirName = "~out"
)

// Options configure a Handler.
type Options struct {
	// Engine is Find's setting: a program, a folder, "auto", or "" for
	// none.
	Engine string
	// Owner is the agent's node ID, put on every container it starts.
	Owner string
	// Start starts the engine program tied to the agent's life (the
	// agent's startProgram); nil starts it plainly.
	Start func(*exec.Cmd) (<-chan error, error)
	// MaxOutBytes bounds /out (default DefaultMaxOutBytes). ProbeTTL is
	// how long a probe is reused, WatchEvery how often /out is measured
	// (tests shorten both).
	MaxOutBytes int64
	ProbeTTL    time.Duration
	WatchEvery  time.Duration
}

// Handler runs container.run (a tasks.Handler and tasks.Describer).
type Handler struct {
	opts Options

	probeMu sync.Mutex
	probed  time.Time
	info    Info
	err     error

	mu       sync.Mutex
	mine     map[string]bool // containers this process started and hasn't finished with
	swept    bool
	sweeping bool
}

// NewHandler returns a container.run handler.
func NewHandler(opts Options) *Handler {
	if opts.MaxOutBytes <= 0 {
		opts.MaxOutBytes = DefaultMaxOutBytes
	}
	if opts.ProbeTTL <= 0 {
		opts.ProbeTTL = defaultProbeTTL
	}
	if opts.WatchEvery <= 0 {
		opts.WatchEvery = time.Second
	}
	if opts.Start == nil {
		opts.Start = func(cmd *exec.Cmd) (<-chan error, error) {
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			return exited, nil
		}
	}
	return &Handler{opts: opts, mine: map[string]bool{}}
}

// probe is the engine's state, asked at most once per ProbeTTL: it runs
// at every capability probe and before every task. The first time the
// engine answers, the sweep clears what an earlier agent left running.
func (h *Handler) probe(ctx context.Context) (Info, error) {
	h.probeMu.Lock()
	defer h.probeMu.Unlock()
	if !h.probed.IsZero() && time.Since(h.probed) < h.opts.ProbeTTL {
		return h.info, h.err
	}
	eng, ok := Find(h.opts.Engine)
	if !ok {
		h.info, h.err = Info{}, errors.New("no container engine (docker or podman) is installed")
	} else {
		// Not canceled with the caller: the answer is shared, and a task
		// canceled mid-probe mustn't make the engine look down.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		h.info, h.err = eng.probe(pctx)
		cancel()
	}
	h.probed = time.Now()
	if h.err == nil {
		go h.sweep(h.info.Engine)
	}
	return h.info, h.err
}

// Available reports whether the engine answers and runs Linux containers.
func (h *Handler) Available(ctx context.Context) error {
	_, err := h.probe(ctx)
	return err
}

// Attributes advertise the engine and its platform, which a task's
// platform parameter is matched against.
func (h *Handler) Attributes(ctx context.Context) map[string]string {
	info, err := h.probe(ctx)
	if err != nil {
		return nil
	}
	return map[string]string{catalog.AttrEngine: info.Engine.Kind + " " + info.Version, catalog.AttrPlatform: info.Platform}
}

// Sweep removes containers an earlier run of this agent left behind (it
// was killed, crashed or updated while they ran: killing the engine's
// command-line program doesn't stop its container). Only containers
// labelled with this agent's node ID are touched, never one this process
// is running.
func (h *Handler) Sweep(ctx context.Context) {
	if info, err := h.probe(ctx); err == nil {
		h.sweep(info.Engine)
	}
}

var nameRe = regexp.MustCompile(`^hh-[0-9a-f]{12}-[0-9a-f]{8}$`)

func (h *Handler) sweep(eng Engine) {
	if h.opts.Owner == "" {
		return
	}
	h.mu.Lock()
	if h.swept || h.sweeping {
		h.mu.Unlock()
		return
	}
	h.sweeping = true
	h.mu.Unlock()
	done := false
	defer func() {
		h.mu.Lock()
		h.sweeping, h.swept = false, h.swept || done
		h.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := eng.command(ctx, "ps", "-a", "--filter", "label="+LabelOwner+"="+h.opts.Owner, "--format", "{{.Names}}").Output()
	if err != nil {
		return
	}
	var stale []string
	h.mu.Lock()
	for _, name := range strings.Fields(string(out)) {
		if nameRe.MatchString(name) && !h.mine[name] {
			stale = append(stale, name)
		}
	}
	h.mu.Unlock()
	if len(stale) > 0 {
		log.Printf("container: removing %d container(s) left running by an earlier agent: %s", len(stale), strings.Join(stale, ", "))
		if err := eng.command(ctx, append([]string{"rm", "-f"}, stale...)...).Run(); err != nil {
			log.Printf("container: remove leftovers: %v", err)
			return
		}
	}
	done = true
}

func (h *Handler) track(name string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on {
		h.mine[name] = true
	} else {
		delete(h.mine, name)
	}
}

// resweep has the next probe sweep again: a container may have been
// created after its cancel could kill it.
func (h *Handler) resweep() {
	h.mu.Lock()
	h.swept = false
	h.mu.Unlock()
	h.probeMu.Lock()
	h.probed = time.Time{}
	h.probeMu.Unlock()
}

func containerName(wl domain.WorkloadID) string {
	var b [10]byte
	rand.Read(b[:])
	r := hex.EncodeToString(b[:])
	prefix := r[8:]
	if domain.ValidWorkloadID(wl) {
		prefix = string(wl)[:12]
	}
	return "hh-" + prefix + "-" + r[:8]
}

var errTimeLimit = errors.New("time limit")

// Run runs one container.run task.
func (h *Handler) Run(ctx context.Context, env tasks.Env) error {
	info, err := h.probe(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("can't run containers here: %w", err)
	}
	eng := info.Engine
	img, err := catalog.ParseImage(env.Params["image"])
	if err != nil {
		return err
	}
	args, err := catalog.ContainerArgs(env.Params["args"])
	if err != nil {
		return fmt.Errorf("args: %w", err)
	}
	memMB, _ := strconv.Atoi(env.Params["memory_mb"])
	timeout, _ := strconv.Atoi(env.Params["timeout"])
	if memMB <= 0 || timeout <= 0 || env.Params["cpus"] == "" {
		return errors.New("cpus, memory_mb and timeout must be set (an assignment from an older manager?)")
	}
	spec := Spec{Name: containerName(env.Workload), Owner: h.opts.Owner, Workload: string(env.Workload), Image: img.String(), Args: args,
		CPUs: env.Params["cpus"], MemoryMB: memMB, Network: env.Params["network"], Platform: env.Params["platform"]}
	spec.User, spec.KeepID = runAs(eng.Kind)
	if env.Dir != "" {
		if spec.InDir, spec.OutDir, err = stage(env.Dir, env.Inputs, env.Outputs); err != nil {
			return err
		}
	}

	h.track(spec.Name, true)
	defer h.track(spec.Name, false)
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	runCtx, cancelLimit := context.WithTimeoutCause(runCtx, time.Duration(timeout)*time.Second, errTimeLimit)
	defer cancelLimit()

	cmd := eng.command(runCtx, eng.RunArgs(spec)...)
	cmd.Stdout, cmd.Stderr = env.Stdout, env.Stderr
	// Killing the engine's program would leave its container running:
	// cancel, a timeout or a full /out kill the container itself, and
	// the program then exits on its own.
	var killFailed atomic.Bool
	cmd.Cancel = func() error {
		if err := eng.kill(spec.Name); err != nil {
			// No container yet (still pulling the image) or already gone.
			killFailed.Store(true)
			cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 15 * time.Second
	exited, err := h.opts.Start(cmd)
	if err != nil {
		return fmt.Errorf("start %s: %w", eng.Kind, err)
	}
	if spec.OutDir != "" {
		go watchOut(runCtx, spec.OutDir, h.opts.MaxOutBytes, h.opts.WatchEvery, stop)
	}
	runErr := <-exited
	if killFailed.Load() {
		eng.remove(spec.Name) // one created after the kill was tried
		h.resweep()
	}
	cause := context.Cause(runCtx)
	switch {
	case ctx.Err() != nil:
		return ctx.Err() // canceled, or the workload's own deadline
	case errors.Is(cause, errTimeLimit):
		return fmt.Errorf("stopped: the container ran past its time limit (%ds)", timeout)
	case runCtx.Err() != nil && cause != nil:
		return cause
	case runErr != nil:
		return exitError(runErr)
	}
	if spec.OutDir == "" {
		return nil
	}
	return collect(spec.OutDir, env.Dir, env.Outputs, h.opts.MaxOutBytes)
}

func (e Engine) kill(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return e.command(ctx, "kill", name).Run()
}

func (e Engine) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	e.command(ctx, "rm", "-f", name).Run()
}

// exitErr words the engine's exit codes, keeping the *exec.ExitError
// underneath for the workload's exit code.
type exitErr struct {
	msg string
	err error
}

func (e exitErr) Error() string { return e.msg }
func (e exitErr) Unwrap() error { return e.err }

func exitError(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	switch code := ee.ExitCode(); code {
	case 125:
		return exitErr{"the container engine couldn't run it (exit 125; its reason is in stderr)", err}
	case 126:
		return exitErr{"the container's command couldn't be started (exit 126)", err}
	case 127:
		return exitErr{"the container's command wasn't found (exit 127)", err}
	default:
		return exitErr{fmt.Sprintf("the container exited with code %d", code), err}
	}
}

// stage lays out the working directory for the mounts: the inputs move
// into dir/~in (readable by the container's user), and dir/~out starts
// empty, with the outputs' folders made, writable by it.
func stage(dir string, inputs, outputs []string) (in, out string, err error) {
	if strings.ContainsAny(dir, `,"`) {
		return "", "", fmt.Errorf("the working directory %s has a ',' or '\"', which a container mount can't take: give the agent another -work-dir", dir)
	}
	if len(inputs) > 0 {
		in = filepath.Join(dir, inDirName)
		for _, name := range inputs {
			dst := filepath.Join(in, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return "", "", err
			}
			if err := os.Rename(filepath.Join(dir, filepath.FromSlash(name)), dst); err != nil {
				return "", "", fmt.Errorf("input %s: %w", name, err)
			}
		}
		// The container runs as another user where the agent can't pick
		// its own (Windows, a root agent): world-readable, inside the
		// agent's private working directory.
		err := filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return os.Chmod(p, 0o755)
			}
			return os.Chmod(p, 0o644)
		})
		if err != nil {
			return "", "", err
		}
	}
	if len(outputs) > 0 {
		out = filepath.Join(dir, outDirName)
		for _, d := range append([]string{out}, outputParents(out, outputs)...) {
			if err := os.MkdirAll(d, 0o777); err != nil {
				return "", "", err
			}
			if err := os.Chmod(d, 0o777); err != nil {
				return "", "", err
			}
		}
	}
	return in, out, nil
}

func outputParents(out string, outputs []string) []string {
	var dirs []string
	for _, name := range outputs {
		for d := filepath.Dir(filepath.FromSlash(name)); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
			dirs = append(dirs, filepath.Join(out, d))
		}
	}
	return dirs
}

// watchOut stops the container (through stop) once /out holds more than
// max bytes or too many files: it is the device's own disk.
func watchOut(ctx context.Context, out string, max int64, every time.Duration, stop context.CancelCauseFunc) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var size int64
		files := 0
		filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			files++
			// Lstat, not d.Info(): on Windows a directory listing shows a
			// file still being written at its old size.
			if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
				size += fi.Size()
			}
			if size > max || files > maxOutFiles {
				return filepath.SkipAll
			}
			return nil
		})
		if size > max {
			stop(fmt.Errorf("stopped: the container wrote more than %d MB to /out", max>>20))
			return
		}
		if files > maxOutFiles {
			stop(fmt.Errorf("stopped: the container wrote more than %d files to /out", maxOutFiles))
			return
		}
	}
}

// collect copies the declared outputs from out into dir, where the
// agent uploads them from. The container wrote out: everything is read
// through an os.Root, so a symlink it made (to a host file, or a
// symlinked folder on the way) can't make the agent read outside out,
// and only regular files are taken.
func collect(out, dir string, names []string, max int64) error {
	root, err := os.OpenRoot(out)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	for _, name := range names {
		rel := filepath.FromSlash(name)
		fi, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("the container didn't write /out/%s", name)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("/out/%s is not a regular file", name)
		}
		if total += fi.Size(); total > max {
			return fmt.Errorf("the outputs are larger than %d MB", max>>20)
		}
		if err := copyOut(root, rel, filepath.Join(dir, rel), fi.Size()); err != nil {
			return fmt.Errorf("output %s: %w", name, err)
		}
	}
	return nil
}

func copyOut(root *os.Root, rel, dst string, size int64) error {
	src, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer src.Close()
	if fi, err := src.Stat(); err != nil || !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(src, size)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
