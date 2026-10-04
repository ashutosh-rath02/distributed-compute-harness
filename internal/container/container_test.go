package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/tasks"
)

var fakeBuild struct {
	once sync.Once
	path string
	err  error
	out  []byte
}

// fakeDocker puts the docker stand-in (test/integration/testdata/
// fakedocker) in a fresh folder of its own: it keeps its log and its
// containers next to itself.
func fakeDocker(t *testing.T) string {
	t.Helper()
	fakeBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "fakedocker-build")
		if err != nil {
			fakeBuild.err = err
			return
		}
		fakeBuild.path = filepath.Join(dir, "docker"+exeSuffix())
		fakeBuild.out, fakeBuild.err = exec.Command("go", "build", "-o", fakeBuild.path, "../../test/integration/testdata/fakedocker").CombinedOutput()
	})
	if fakeBuild.err != nil {
		t.Fatalf("build fakedocker: %v\n%s", fakeBuild.err, fakeBuild.out)
	}
	dir := t.TempDir()
	b, err := os.ReadFile(fakeBuild.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"+exeSuffix()), b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// calls is the fake's log: every command line it was run with.
func calls(t *testing.T, dir string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c struct{ Args []string }
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			out = append(out, c.Args)
		}
	}
	return out
}

func callsOf(t *testing.T, dir, verb string) [][]string {
	var out [][]string
	for _, c := range calls(t, dir) {
		if len(c) > 0 && c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func flagValue(args []string, flag string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}

const testImage = "docker.io/library/alpine@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func params(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	p := map[string]string{"image": testImage}
	for k, v := range extra {
		p[k] = v
	}
	ty, _ := catalog.Lookup(catalog.ContainerRun)
	canon, _, err := ty.Compile(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// newTestHandler is a handler on the stand-in, probed once up front (the
// stand-in's first start can be slow while the OS scans the new file).
func newTestHandler(t *testing.T, engineDir string) *Handler {
	t.Helper()
	h := NewHandler(Options{Engine: engineDir, Owner: "node-test", MaxOutBytes: 1 << 20, WatchEvery: 50 * time.Millisecond, ProbeTTL: time.Minute})
	if err := h.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

// workDir lays out a workload's working directory as the agent does:
// inputs fetched into it by name, output folders made.
func workDir(t *testing.T, inputs map[string]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var names []string
	for name, content := range inputs {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return dir, names
}

func TestRunArgsAreHardened(t *testing.T) {
	spec := Spec{Name: "hh-0123456789ab-cdef0123", Owner: "node-1", Workload: "w1", Image: testImage, Args: []string{"--privileged", "-v", "/:/host"},
		CPUs: "0.5", MemoryMB: 256, Network: "bridge", Platform: "linux/arm64", User: "1000:1000", InDir: "/work/w1/~in", OutDir: "/work/w1/~out"}
	got := Engine{Path: "/usr/bin/docker", Kind: "docker"}.RunArgs(spec)
	want := []string{"run", "--rm", "--name=hh-0123456789ab-cdef0123", "--label=home-harness.agent=node-1", "--label=home-harness.workload=w1",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--user=1000:1000",
		"--network=bridge", "--cpus=0.5", "--memory=256m", "--memory-swap=256m", "--log-driver=none", "--platform=linux/arm64",
		"--mount=type=bind,source=/work/w1/~in,target=/in,readonly", "--mount=type=bind,source=/work/w1/~out,target=/out",
		testImage, "--privileged", "-v", "/:/host"}
	if !slices.Equal(got, want) {
		t.Fatalf("docker run argv:\n got %q\nwant %q", got, want)
	}
	// The task's own arguments come only after the image: whatever they
	// say, the engine reads them as the container's command line.
	if i := slices.Index(got, testImage); i != len(got)-4 {
		t.Fatalf("image at %d of %d", i, len(got))
	}

	// Podman: the agent's user mapped in, image volumes ignored; no
	// network unless "bridge"; no mounts without files.
	spec.Network, spec.KeepID, spec.InDir, spec.OutDir, spec.Platform, spec.Args = "host", true, "", "", "", nil
	got = Engine{Path: "/usr/bin/podman", Kind: "podman"}.RunArgs(spec)
	for _, w := range []string{"--userns=keep-id", "--image-volume=ignore", "--network=none"} {
		if !slices.Contains(got, w) {
			t.Errorf("podman argv lacks %s: %q", w, got)
		}
	}
	for _, a := range got {
		if strings.HasPrefix(a, "--mount") || strings.HasPrefix(a, "--platform") || a == "--network=host" {
			t.Errorf("unexpected %s in %q", a, got)
		}
	}
	if got[len(got)-1] != testImage {
		t.Fatalf("argv should end with the image: %q", got)
	}
}

func TestFindLooksOnlyAtAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Find(dir); ok {
		t.Fatal("found an engine in an empty folder")
	}
	podman := filepath.Join(dir, "podman"+exeSuffix())
	os.WriteFile(podman, []byte("x"), 0o755)
	if e, ok := Find(dir); !ok || e.Kind != "podman" || e.Path != podman {
		t.Fatalf("podman only: %+v %v", e, ok)
	}
	docker := filepath.Join(dir, "docker"+exeSuffix())
	os.WriteFile(docker, []byte("x"), 0o755)
	if e, ok := Find(dir); !ok || e.Kind != "docker" || e.Path != docker {
		t.Fatalf("docker first: %+v %v", e, ok)
	}
	if e, ok := Find(podman); !ok || e.Kind != "podman" {
		t.Fatalf("a program given directly: %+v %v", e, ok)
	}
	for _, s := range []string{"", "off", "docker", filepath.Join(dir, "missing")} {
		if e, ok := Find(s); ok {
			t.Errorf("Find(%q) = %+v", s, e)
		}
	}
	for _, p := range standardLocations() {
		if !filepath.IsAbs(p) {
			t.Errorf("standard location %s isn't absolute", p)
		}
	}
}

func TestEngineFolderGoesFirstOnPath(t *testing.T) {
	env := withPathDir([]string{"HOME=/h", "PATH=/usr/bin"}, "/opt/docker")
	want := "PATH=/opt/docker" + string(os.PathListSeparator) + "/usr/bin"
	if env[1] != want || len(env) != 2 {
		t.Fatalf("env %q", env)
	}
	if env := withPathDir([]string{"HOME=/h"}, "/opt/docker"); env[1] != "PATH=/opt/docker" {
		t.Fatalf("no PATH: %q", env)
	}
}

func TestProbeNeedsAnAnsweringLinuxEngine(t *testing.T) {
	dir := fakeDocker(t)
	h := NewHandler(Options{Engine: dir, ProbeTTL: time.Millisecond})
	if err := h.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	attrs := h.Attributes(context.Background())
	if attrs[catalog.AttrEngine] != "docker 99.0.0-fake" || attrs[catalog.AttrPlatform] != "linux/amd64" {
		t.Fatalf("attributes %v", attrs)
	}
	os.WriteFile(filepath.Join(dir, "daemon-down"), nil, 0o600)
	time.Sleep(5 * time.Millisecond)
	if err := h.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("daemon down: %v", err)
	}
	os.Remove(filepath.Join(dir, "daemon-down"))
	os.WriteFile(filepath.Join(dir, "platform"), []byte("windows/amd64"), 0o600)
	time.Sleep(5 * time.Millisecond)
	if err := h.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "Linux containers") {
		t.Fatalf("windows containers: %v", err)
	}
	if err := NewHandler(Options{Engine: t.TempDir()}).Available(context.Background()); err == nil {
		t.Fatal("no engine installed, yet available")
	}
}

func TestRunMountsInputsReadOnlyAndBringsBackOutputs(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine)
	dir, inputs := workDir(t, map[string]string{"data.txt": "hello container\n", "sub/more.txt": "x"})
	var stdout, stderr bytes.Buffer
	env := tasks.Env{Dir: dir, Inputs: inputs, Outputs: []string{"result.txt", "deep/copy.txt"}, Stdout: &stdout, Stderr: &stderr, Workload: "0123456789abcdef0123456789abcdef",
		Params: params(t, map[string]string{"args": `["copy","/in/data.txt","/out/result.txt"]`, "outputs": "result.txt,deep/copy.txt"})}
	// Two outputs, one command: the second is checked as missing below.
	err := h.Run(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "didn't write /out/deep/copy.txt") {
		t.Fatalf("a declared output it didn't write: %v", err)
	}
	runs := callsOf(t, engine, "run")
	if len(runs) != 1 {
		t.Fatalf("runs %q", runs)
	}
	args := runs[0]
	var mounts []string
	for _, a := range args {
		if strings.HasPrefix(a, "--mount") || strings.HasPrefix(a, "-v") || strings.HasPrefix(a, "--volume") {
			mounts = append(mounts, a)
		}
	}
	wantMounts := []string{
		"--mount=type=bind,source=" + filepath.Join(dir, "~in") + ",target=/in,readonly",
		"--mount=type=bind,source=" + filepath.Join(dir, "~out") + ",target=/out",
	}
	if !slices.Equal(mounts, wantMounts) {
		t.Fatalf("mounts %q, want %q", mounts, wantMounts)
	}
	for _, f := range []string{"--rm", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--network=none", "--cpus=1", "--memory=512m", "--memory-swap=512m"} {
		if !slices.Contains(args, f) {
			t.Errorf("run lacks %s: %q", f, args)
		}
	}
	if u := flagValue(args, "--user"); u == "" || strings.HasPrefix(u, "0:") || u == "0" {
		t.Errorf("container user %q", u)
	}
	if n := flagValue(args, "--name"); !nameRe.MatchString(n) || !strings.HasPrefix(n, "hh-0123456789ab-") {
		t.Errorf("name %q", n)
	}
	if args[len(args)-4] != testImage {
		t.Errorf("image not right before the command: %q", args)
	}

	// Again with just the output it writes.
	dir, inputs = workDir(t, map[string]string{"data.txt": "hello container\n"})
	env = tasks.Env{Dir: dir, Inputs: inputs, Outputs: []string{"result.txt"}, Stdout: &stdout, Stderr: &stderr,
		Params: params(t, map[string]string{"args": `["copy","/in/data.txt","/out/result.txt"]`, "outputs": "result.txt"})}
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatalf("run: %v (stderr %s)", err, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.txt"))
	if err != nil || string(b) != "hello container\n" {
		t.Fatalf("output %q %v", b, err)
	}
	// /in is read-only: the fake refuses writes there like the engine.
	dir, inputs = workDir(t, map[string]string{"data.txt": "x"})
	env = tasks.Env{Dir: dir, Inputs: inputs, Outputs: []string{"result.txt"}, Stdout: io.Discard, Stderr: &stderr,
		Params: params(t, map[string]string{"args": `["copy","/in/data.txt","/in/evil.txt"]`, "outputs": "result.txt"})}
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(stderr.String(), "Read-only file system") {
		t.Fatalf("write to /in: %v %s", err, stderr.String())
	}
}

func TestRunWithoutFilesMountsNothing(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine)
	var stdout bytes.Buffer
	env := tasks.Env{Params: params(t, map[string]string{"args": `["echo","hi","--from","-the-container"]`}), Stdout: &stdout, Stderr: io.Discard}
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hi --from -the-container\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
	for _, a := range callsOf(t, engine, "run")[0] {
		if strings.HasPrefix(a, "--mount") {
			t.Fatalf("mount without files: %s", a)
		}
	}
}

func TestRunReportsExitCodes(t *testing.T) {
	h := newTestHandler(t, fakeDocker(t))
	for args, want := range map[string]string{`["exit","3"]`: "exited with code 3", `["exit","125"]`: "engine couldn't run it", `["nope"]`: "wasn't found"} {
		err := h.Run(context.Background(), tasks.Env{Params: params(t, map[string]string{"args": args}), Stdout: io.Discard, Stderr: io.Discard})
		var ee *exec.ExitError
		if err == nil || !strings.Contains(err.Error(), want) || !errors.As(err, &ee) {
			t.Errorf("%s: %v", args, err)
		}
	}
}

// killedName waits for the fake to log a kill and returns its target.
func killedName(t *testing.T, dir string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if k := callsOf(t, dir, "kill"); len(k) > 0 {
			return k[0][1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no docker kill")
	return ""
}

func TestCancelKillsTheContainer(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	err := h.Run(ctx, tasks.Env{Params: params(t, map[string]string{"args": `["sleep","30"]`}), Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Fatalf("canceled run: %v after %v", err, time.Since(start))
	}
	if name := killedName(t, engine); name != flagValue(callsOf(t, engine, "run")[0], "--name") {
		t.Fatalf("killed %q", name)
	}
	if entries, _ := os.ReadDir(filepath.Join(engine, "containers")); len(entries) != 0 {
		t.Fatalf("container left: %v", entries)
	}
}

func TestTimeLimitKillsTheContainer(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine)
	err := h.Run(context.Background(), tasks.Env{Params: params(t, map[string]string{"args": `["sleep","30"]`, "timeout": "1"}), Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "time limit (1s)") {
		t.Fatalf("over the time limit: %v", err)
	}
	killedName(t, engine)
}

func TestCancelWhilePullingStopsTheProgram(t *testing.T) {
	engine := fakeDocker(t)
	os.WriteFile(filepath.Join(engine, "pull-delay"), []byte("20000"), 0o600)
	h := newTestHandler(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	err := h.Run(ctx, tasks.Env{Params: params(t, map[string]string{"args": `["sleep","30"]`}), Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Fatalf("canceled while pulling: %v after %v", err, time.Since(start))
	}
	// The kill found no container yet: the program was stopped and the
	// name removed in case the engine created it anyway.
	if rm := callsOf(t, engine, "rm"); len(rm) != 1 || rm[0][2] != flagValue(callsOf(t, engine, "run")[0], "--name") {
		t.Fatalf("rm calls %q", rm)
	}
}

func TestOutIsCappedWhileItRuns(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine) // 1 MiB
	dir, _ := workDir(t, nil)
	err := h.Run(context.Background(), tasks.Env{Dir: dir, Outputs: []string{"big.bin"}, Stdout: io.Discard, Stderr: io.Discard,
		Params: params(t, map[string]string{"args": `["fill","/out/big.bin","104857600"]`, "outputs": "big.bin"})})
	if err == nil || !strings.Contains(err.Error(), "more than 1 MB to /out") {
		t.Fatalf("filling /out: %v", err)
	}
	killedName(t, engine)
}

func TestOutputsAreReadOnlyInsideOut(t *testing.T) {
	engine := fakeDocker(t)
	h := newTestHandler(t, engine)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	os.WriteFile(secret, []byte("host secret"), 0o600)
	probe := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(secret, probe); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	for _, c := range []struct{ target, link, output string }{
		{secret, "/out/result.txt", "result.txt"},                         // a symlink to a host file
		{secretDir, "/out/sub", "sub/secret.txt"},                         // a symlinked folder on the way
		{"../../" + filepath.Base(secretDir), "/out/up", "up/secret.txt"}, // a relative escape
	} {
		dir, _ := workDir(t, nil)
		err := h.Run(context.Background(), tasks.Env{Dir: dir, Outputs: []string{c.output}, Stdout: io.Discard, Stderr: io.Discard,
			Params: params(t, map[string]string{"args": `["symlink",` + jsonString(c.target) + `,"` + c.link + `"]`, "outputs": c.output})})
		if err == nil {
			t.Errorf("%s -> %s: no error", c.link, c.target)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.output))); strings.Contains(string(b), "host secret") {
			t.Errorf("%s -> %s: the host file was copied out", c.link, c.target)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// linkDir makes link point at the folder target: a symlink, or on Windows
// without the symlink privilege a junction (which needs none).
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if os.Symlink(target, link) == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("no symlink or junction: %v %s", err, out)
		}
	}
	t.Skip("symlinks unavailable here")
}

// Whatever links the container left in /out, collecting outputs reads
// only inside it.
func TestCollectNeverLeavesOut(t *testing.T) {
	secretDir := t.TempDir()
	os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("host secret"), 0o600)
	out := filepath.Join(t.TempDir(), outDirName)
	os.Mkdir(out, 0o777)
	linkDir(t, secretDir, filepath.Join(out, "sub"))
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, "sub"), 0o700)
	if err := collect(out, dst, []string{"sub/secret.txt"}, 1<<20); err == nil {
		t.Fatal("collected through a link out of /out")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "secret.txt")); len(b) > 0 {
		t.Fatalf("copied %q", b)
	}
	// A regular file beside it is collected.
	os.WriteFile(filepath.Join(out, "ok.txt"), []byte("fine"), 0o666)
	if err := collect(out, dst, []string{"ok.txt"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ok.txt")); string(b) != "fine" {
		t.Fatalf("collected %q", b)
	}
	// And the size cap holds for what is collected.
	os.WriteFile(filepath.Join(out, "big.txt"), make([]byte, 2<<20), 0o666)
	if err := collect(out, t.TempDir(), []string{"big.txt"}, 1<<20); err == nil {
		t.Fatal("collected more than the cap")
	}
}

func TestSweepRemovesOnlyThisAgentsLeftovers(t *testing.T) {
	engine := fakeDocker(t)
	os.MkdirAll(filepath.Join(engine, "containers"), 0o700)
	put := func(name, owner string) {
		b, _ := json.Marshal(map[string]any{"name": name, "labels": map[string]string{LabelOwner: owner}})
		os.WriteFile(filepath.Join(engine, "containers", name+".json"), b, 0o600)
	}
	put("hh-aaaaaaaaaaaa-11111111", "node-test")  // left by an earlier run of this agent
	put("hh-bbbbbbbbbbbb-22222222", "node-other") // another agent's
	put("someones-db", "node-test")               // not a name this agent gives
	h := newTestHandler(t, engine)
	h.Sweep(context.Background())
	waitSwept(t, h)

	// A container this process is running is never swept, even if a
	// sweep happens while it runs.
	done := make(chan error, 1)
	go func() {
		done <- h.Run(context.Background(), tasks.Env{Params: params(t, map[string]string{"args": `["sleep","1"]`}), Stdout: io.Discard, Stderr: io.Discard})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(callsOf(t, engine, "run")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	h.swept = false
	h.mu.Unlock()
	h.sweep(Engine{Path: filepath.Join(engine, "docker"+exeSuffix()), Kind: "docker"})
	if !h.swept {
		t.Fatal("the sweep during the run didn't complete")
	}
	if err := <-done; err != nil {
		t.Fatalf("the running container was disturbed: %v", err)
	}
	var removed []string
	for _, c := range callsOf(t, engine, "rm") {
		removed = append(removed, c[2:]...)
	}
	if !slices.Equal(removed, []string{"hh-aaaaaaaaaaaa-11111111"}) {
		t.Fatalf("removed %q", removed)
	}
	for _, ps := range callsOf(t, engine, "ps") {
		if !slices.Contains(ps, "label=home-harness.agent=node-test") {
			t.Fatalf("ps without the owner filter: %q", ps)
		}
	}
}

// waitSwept waits for a sweep (perhaps one a probe started) to finish.
func waitSwept(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := h.swept && !h.sweeping
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no sweep")
}

func TestStagingRefusesPathsAMountCantTake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a,b")
	os.MkdirAll(dir, 0o700)
	if _, _, err := stage(dir, nil, []string{"x"}); err == nil {
		t.Fatal("a comma in the working directory's path")
	}
	if runtime.GOOS == "windows" {
		return // '"' can't be in a Windows path at all
	}
	dir = filepath.Join(t.TempDir(), `a"b`)
	os.MkdirAll(dir, 0o700)
	if _, _, err := stage(dir, nil, []string{"x"}); err == nil {
		t.Fatal("a quote in the working directory's path")
	}
}

func TestContainerNamesAreUniquePerAttempt(t *testing.T) {
	wl := domain.WorkloadID("0123456789abcdef0123456789abcdef")
	a, b := containerName(wl), containerName(wl)
	if a == b || !nameRe.MatchString(a) || !strings.HasPrefix(a, "hh-0123456789ab-") || !nameRe.MatchString(containerName("")) {
		t.Fatalf("names %s %s %s", a, b, containerName(""))
	}
}
