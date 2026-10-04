package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/transport/ws"
)

// Container tasks (roadmap item 13) against a stand-in docker
// (testdata/fakedocker) that logs every command line it gets. No real
// container engine is involved: these tests check what the agent asks of
// one, and what the manager lets through.

const containerDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var fakeDockerBuild struct {
	once sync.Once
	path string
	out  []byte
	err  error
}

// fakeDockerDir puts the docker stand-in in a folder of its own (it keeps
// its log and "containers" next to itself), answering as platform.
func fakeDockerDir(t *testing.T, platform string) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	fakeDockerBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "fakedocker-build")
		if err != nil {
			fakeDockerBuild.err = err
			return
		}
		fakeDockerBuild.path = filepath.Join(dir, "docker"+ext)
		fakeDockerBuild.out, fakeDockerBuild.err = exec.Command("go", "build", "-o", fakeDockerBuild.path, "./testdata/fakedocker").CombinedOutput()
	})
	if fakeDockerBuild.err != nil {
		t.Fatalf("build fakedocker: %v\n%s", fakeDockerBuild.err, fakeDockerBuild.out)
	}
	dir := t.TempDir()
	b, err := os.ReadFile(fakeDockerBuild.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"+ext), b, 0o755); err != nil {
		t.Fatal(err)
	}
	if platform != "" {
		os.WriteFile(filepath.Join(dir, "platform"), []byte(platform), 0o600)
	}
	return dir
}

// dockerCalls is the stand-in's log of command lines starting with verb.
func dockerCalls(t *testing.T, dir, verb string) [][]string {
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
		if json.Unmarshal(sc.Bytes(), &c) == nil && len(c.Args) > 0 && c.Args[0] == verb {
			out = append(out, c.Args)
		}
	}
	return out
}

func argValue(args []string, flag string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}

func newContainerAgent(t *testing.T, addr, name, engine string) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, OllamaURL: "127.0.0.1:1", ContainerEngine: engine, WorkloadSlots: 2,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
		HostFingerprint: "-", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// startContainerAgent runs an agent whose container engine is engine
// ("" = none) and waits until it is ready.
func startContainerAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name, engine string) *agent.Agent {
	t.Helper()
	a := newContainerAgent(t, addr, name, engine)
	go a.Run(ctx)
	waitFor(t, 15*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	return a
}

func containerPolicy(tp domain.TypePolicy) *domain.Policy {
	p := domain.DefaultPolicy()
	p.Types[catalog.ContainerRun] = tp
	return &p
}

func containerTask(params map[string]string) map[string]any {
	p := map[string]string{"image": "alpine@" + containerDigest}
	for k, v := range params {
		p[k] = v
	}
	return map[string]any{"capability": "container.run", "params": p}
}

func TestContainerRunsHardenedWithItsFiles(t *testing.T) {
	const addr = "127.0.0.1:19670"
	m := startPolicyManager(t, addr, containerPolicy(domain.TypePolicy{Enabled: true, AllowImages: []string{"docker.io/library/alpine"}}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	engine := fakeDockerDir(t, "")
	a := startContainerAgent(t, ctx, m, addr, "container-pc", engine)

	sha := uploadArtifact(t, m.api, []byte("hello container\n"))
	body := containerTask(map[string]string{"args": `["copy","/in/data.txt","/out/result.txt"]`, "outputs": "result.txt", "memory_mb": "256"})
	body["inputs"] = []map[string]string{{"name": "data.txt", "sha256": sha}}
	code, out := postWorkload(t, m.api, body)
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["id"].(string)
	v := getFileWorkload(t, m.api, id)
	waitFor(t, 30*time.Second, func() bool {
		v = getFileWorkload(t, m.api, id)
		return v.State == domain.WorkloadCompleted || v.State == domain.WorkloadFailed
	})
	if v.State != domain.WorkloadCompleted || len(v.OutputFiles) != 1 || v.OutputFiles[0].Name != "result.txt" {
		t.Fatalf("container task: %s (%s) %s outputs %+v", v.State, v.Error, v.Stderr, v.OutputFiles)
	}
	resp, err := http.Get(m.api + "/artifacts/" + v.OutputFiles[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "hello container\n" {
		t.Fatalf("output %q", got)
	}
	if rec, _ := m.srv.Workloads.Get(domain.WorkloadID(id)); rec.Workload.Requirements.MinMemoryBytes != 256<<20 || rec.Workload.Requirements.MinCPUCores != 1 {
		t.Fatalf("reservation %+v (want the container's own limits)", rec.Workload.Requirements)
	}

	// What the agent asked of the engine: exactly one run, hardened, the
	// image canonical, the task's command after it, and only the
	// workload's own in (read-only) and out mounted.
	runs := dockerCalls(t, engine, "run")
	if len(runs) != 1 {
		t.Fatalf("runs %q", runs)
	}
	args := runs[0]
	for _, f := range []string{"--rm", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--network=none",
		"--cpus=1", "--memory=256m", "--memory-swap=256m", "--log-driver=none",
		"--label=home-harness.agent=" + string(a.NodeID()), "--label=home-harness.workload=" + id} {
		if !slices.Contains(args, f) {
			t.Errorf("run lacks %s", f)
		}
	}
	if u := argValue(args, "--user"); u == "" || u == "0" || strings.HasPrefix(u, "0:") || u == "root" {
		t.Errorf("container user %q", u)
	}
	var mounts []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--mount") || strings.HasPrefix(arg, "-v") || strings.HasPrefix(arg, "--volume") || strings.Contains(arg, "privileged") ||
			strings.HasPrefix(arg, "--device") || strings.HasPrefix(arg, "--cap-add") || arg == "--network=host" {
			mounts = append(mounts, arg)
		}
	}
	if len(mounts) != 2 || !strings.HasPrefix(mounts[0], "--mount=type=bind,source=") || !strings.HasSuffix(mounts[0], string(filepath.Separator)+"~in,target=/in,readonly") ||
		!strings.HasSuffix(mounts[1], string(filepath.Separator)+"~out,target=/out") || !strings.Contains(mounts[0], id) {
		t.Fatalf("mounts and privileges %q", mounts)
	}
	tail := []string{"docker.io/library/alpine@" + containerDigest, "copy", "/in/data.txt", "/out/result.txt"}
	if !slices.Equal(args[len(args)-4:], tail) {
		t.Fatalf("image and command %q", args[len(args)-4:])
	}
}

func TestContainerPolicyGatesSubmissions(t *testing.T) {
	const addr = "127.0.0.1:19671"
	p := domain.DefaultPolicy()
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	engine := fakeDockerDir(t, "")
	startContainerAgent(t, ctx, m, addr, "gate-pc", engine)

	refused := func(what string, body map[string]any, says string) {
		t.Helper()
		code, out := postWorkload(t, m.api, body)
		if code != http.StatusForbidden || !strings.Contains(out["raw"].(string), says) {
			t.Fatalf("%s: %d %v (want 403 saying %s)", what, code, out, says)
		}
	}
	echo := map[string]string{"args": `["echo","hi"]`}
	refused("off by default", containerTask(echo), `"harnessctl policy type container.run on"`)
	if code, _, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{containerTask(echo)}}); code != http.StatusForbidden {
		t.Fatalf("a container job while off: %d %s", code, raw)
	}

	p.Types[catalog.ContainerRun] = domain.TypePolicy{Enabled: true}
	putPolicy(t, m.api, p)
	refused("no images allowed", containerTask(echo), `"harnessctl policy type container.run images add docker.io/library/alpine@`+containerDigest+`"`)

	p.Types[catalog.ContainerRun] = domain.TypePolicy{Enabled: true, AllowImages: []string{"library/*"}}
	putPolicy(t, m.api, p)
	refused("a tag", containerTask(map[string]string{"image": "alpine:3.20"}), `"harnessctl policy type container.run tags on"`)
	refused("network", containerTask(map[string]string{"network": "bridge"}), `"harnessctl policy type container.run network on"`)
	refused("a lookalike namespace", containerTask(map[string]string{"image": "docker.io/library-evil/x@" + containerDigest}), "images add")
	if code, _, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{containerTask(map[string]string{"image": "ghcr.io/me/x@" + containerDigest})}}); code != http.StatusForbidden || !strings.Contains(raw, "images add") {
		t.Fatalf("a job with an image not allowed: %d %s", code, raw)
	}
	if code, out := postWorkload(t, m.api, containerTask(map[string]string{"args": `rm -rf /`})); code != http.StatusBadRequest {
		t.Fatalf("args that aren't a JSON list: %d %v", code, out)
	}

	// The stored list is canonical; the field belongs to container.run.
	var stored domain.Policy
	getJSON(t, m.api+"/policy", &stored)
	if got := stored.Types[catalog.ContainerRun].AllowImages; !slices.Equal(got, []string{"docker.io/library/*"}) {
		t.Fatalf("stored images %q", got)
	}
	bad := domain.DefaultPolicy()
	bad.Types["image.resize"] = domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}}
	b, _ := json.Marshal(bad)
	req, _ := http.NewRequest(http.MethodPut, m.api+"/policy", bytes.NewReader(b))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allowImages on another type: %v %v", resp, err)
	}

	p.Types[catalog.ContainerRun] = domain.TypePolicy{Enabled: true, AllowImages: []string{"library/*"}, AllowNetwork: true}
	putPolicy(t, m.api, p)
	code, out := postWorkload(t, m.api, containerTask(map[string]string{"args": `["echo","online"]`, "network": "bridge"}))
	if code != http.StatusAccepted {
		t.Fatalf("network allowed: %d %v", code, out)
	}
	if v := waitState(t, m.api, out["id"].(string), 30*time.Second); v["state"] != "COMPLETED" || v["stdout"] != "online\n" {
		t.Fatalf("container task: %v", v)
	}
	if runs := dockerCalls(t, engine, "run"); len(runs) != 1 || !slices.Contains(runs[0], "--network=bridge") {
		t.Fatalf("runs %q", runs)
	}
}

// killedFor waits for the stand-in to log a kill of the container whose
// run started with name.
func killedFor(t *testing.T, engine, name string) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		for _, k := range dockerCalls(t, engine, "kill") {
			if len(k) == 2 && k[1] == name {
				return true
			}
		}
		return false
	})
}

func TestContainerCancelAndTimeLimitsKillIt(t *testing.T) {
	const addr = "127.0.0.1:19672"
	tp := domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}, MaxRuntimeSeconds: 600}
	m := startPolicyManager(t, addr, containerPolicy(tp))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	engine := fakeDockerDir(t, "")
	startContainerAgent(t, ctx, m, addr, "kill-pc", engine)

	// lastRun waits for the n-th run and returns its container name.
	lastRun := func(n int) string {
		t.Helper()
		var runs [][]string
		waitFor(t, 15*time.Second, func() bool { runs = dockerCalls(t, engine, "run"); return len(runs) >= n })
		return argValue(runs[n-1], "--name")
	}

	// Cancel.
	code, out := postWorkload(t, m.api, containerTask(map[string]string{"args": `["sleep","60"]`}))
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["id"].(string)
	name := lastRun(1)
	waitFor(t, 10*time.Second, func() bool {
		var v map[string]any
		getJSON(t, m.api+"/workloads/"+id, &v)
		return v["state"] == "RUNNING"
	})
	resp, err := http.Post(m.api+"/workloads/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v := waitState(t, m.api, id, 15*time.Second); v["state"] != "CANCELED" {
		t.Fatalf("canceled: %v", v)
	}
	killedFor(t, engine, name)

	// The task's own time limit.
	start := time.Now()
	_, out = postWorkload(t, m.api, containerTask(map[string]string{"args": `["sleep","60"]`, "timeout": "1"}))
	name = lastRun(2)
	if v := waitState(t, m.api, out["id"].(string), 20*time.Second); v["state"] != "FAILED" || !strings.Contains(v["error"].(string), "time limit (1s)") || time.Since(start) > 15*time.Second {
		t.Fatalf("time limit: %v after %v", v, time.Since(start))
	}
	killedFor(t, engine, name)

	// The policy's max runtime (the workload's own deadline).
	tp.MaxRuntimeSeconds = 1
	putPolicy(t, m.api, *containerPolicy(tp))
	_, out = postWorkload(t, m.api, containerTask(map[string]string{"args": `["sleep","60"]`}))
	name = lastRun(3)
	if v := waitState(t, m.api, out["id"].(string), 20*time.Second); v["state"] != "FAILED" || !strings.Contains(v["error"].(string), "timed out after 1s") {
		t.Fatalf("max runtime: %v", v)
	}
	killedFor(t, engine, name)
	if entries, _ := os.ReadDir(filepath.Join(engine, "containers")); len(entries) != 0 {
		t.Fatalf("containers left: %v", entries)
	}
}

func TestContainersGoOnlyWhereAnEngineAnswers(t *testing.T) {
	const addr = "127.0.0.1:19673"
	m := startPolicyManager(t, addr, containerPolicy(domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	arm := startContainerAgent(t, ctx, m, addr, "arm-box", fakeDockerDir(t, "linux/arm64"))
	amd := startContainerAgent(t, ctx, m, addr, "amd-box", fakeDockerDir(t, "linux/amd64"))
	plain := startContainerAgent(t, ctx, m, addr, "no-engine", "") // an older agent looks the same: it doesn't offer the type

	rec, _ := m.srv.Registry.Get(arm.NodeID())
	var attrs map[string]string
	for _, c := range rec.Capabilities {
		if c.Name == catalog.ContainerRun {
			attrs = c.Attributes
		}
	}
	if attrs[catalog.AttrEngine] != "docker 99.0.0-fake" || attrs[catalog.AttrPlatform] != "linux/arm64" {
		t.Fatalf("advertised %v", attrs)
	}
	var cat struct {
		Types []struct {
			Name  domain.CapabilityName `json:"name"`
			Nodes int                   `json:"nodes"`
		} `json:"types"`
	}
	getJSON(t, m.api+"/catalog", &cat)
	for _, ty := range cat.Types {
		if ty.Name == catalog.ContainerRun && ty.Nodes != 2 {
			t.Fatalf("container.run offered by %d nodes, want 2", ty.Nodes)
		}
	}

	for platform, want := range map[string]domain.NodeID{"linux/arm64": arm.NodeID(), "linux/amd64": amd.NodeID()} {
		code, out := postWorkload(t, m.api, containerTask(map[string]string{"args": `["echo","hi"]`, "platform": platform}))
		if code != http.StatusAccepted || out["target"] != string(want) {
			t.Fatalf("%s went to %v (%d %v)", platform, out["target"], code, out)
		}
	}
	if code, out := postWorkload(t, m.api, containerTask(map[string]string{"platform": "linux/riscv64"})); code != http.StatusConflict {
		t.Fatalf("a platform no device runs: %d %v", code, out)
	}
	body := containerTask(nil)
	body["target"] = plain.NodeID()
	if code, out := postWorkload(t, m.api, body); code != http.StatusConflict {
		t.Fatalf("pinned to a device without an engine: %d %v", code, out)
	}
	for i := 0; i < 3; i++ {
		if code, out := postWorkload(t, m.api, containerTask(nil)); code != http.StatusAccepted || out["target"] == string(plain.NodeID()) {
			t.Fatalf("placed on %v (%d %v)", out["target"], code, out)
		}
	}
}

func TestContainerLeftoversAreSweptAtStart(t *testing.T) {
	const addr = "127.0.0.1:19674"
	startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	engine := fakeDockerDir(t, "")
	a := newContainerAgent(t, addr, "restarted-pc", engine)
	os.MkdirAll(filepath.Join(engine, "containers"), 0o700)
	put := func(name string, owner domain.NodeID) {
		b, _ := json.Marshal(map[string]any{"name": name, "labels": map[string]string{"home-harness.agent": string(owner)}})
		os.WriteFile(filepath.Join(engine, "containers", name+".json"), b, 0o600)
	}
	put("hh-aaaaaaaaaaaa-11111111", a.NodeID()) // left running when this agent last stopped
	put("hh-bbbbbbbbbbbb-22222222", "node-someone-else")
	go a.Run(ctx)
	waitFor(t, 15*time.Second, func() bool {
		for _, c := range dockerCalls(t, engine, "rm") {
			if slices.Contains(c, "hh-aaaaaaaaaaaa-11111111") {
				return true
			}
		}
		return false
	})
	for _, c := range dockerCalls(t, engine, "rm") {
		if slices.Contains(c, "hh-bbbbbbbbbbbb-22222222") {
			t.Fatalf("removed another agent's container: %q", c)
		}
	}
	if _, err := os.Stat(filepath.Join(engine, "containers", "hh-bbbbbbbbbbbb-22222222.json")); err != nil {
		t.Fatal("another agent's container is gone")
	}
}
