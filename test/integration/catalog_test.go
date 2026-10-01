package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func putPolicy(t *testing.T, api string, p domain.Policy) {
	t.Helper()
	b, _ := json.Marshal(p)
	req, _ := http.NewRequest(http.MethodPut, api+"/policy", bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /policy: %s %s", resp.Status, raw)
	}
}

// startPolicyManager is startArtifactManager with a starting policy.
func startPolicyManager(t *testing.T, addr string, p *domain.Policy) artifactManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second,
		ReconcileInterval: 100 * time.Millisecond, Artifacts: openArtifactStore(t, 1<<20), InitialPolicy: p,
	})
	transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	go srv.Run(ctx)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return artifactManager{srv: srv, api: api.URL}
}

func waitState(t *testing.T, api, id string, timeout time.Duration) map[string]any {
	t.Helper()
	var v map[string]any
	waitFor(t, timeout, func() bool {
		getJSON(t, api+"/workloads/"+id, &v)
		s := v["state"]
		return s == "COMPLETED" || s == "FAILED" || s == "CANCELED"
	})
	return v
}

func readyAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name string) *agent.Agent {
	t.Helper()
	a := startFileAgent(t, ctx, m, addr, name)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	return a
}

func TestTypedImageResizeRunsOnARealAgent(t *testing.T) {
	const addr = "127.0.0.1:19550"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "typed-agent")
	sha := uploadArtifact(t, m.api, pngBytes(200, 100))
	code, out := postWorkload(t, m.api, map[string]any{
		"capability": "image.resize", "params": map[string]string{"width": "50", "format": "png"},
		"inputs": []map[string]string{{"name": "photo.png", "sha256": sha}},
	})
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	v := waitState(t, m.api, out["id"].(string), 20*time.Second)
	if v["state"] != "COMPLETED" {
		t.Fatalf("typed task: %v (%v)", v["state"], v["error"])
	}
	files := v["outputFiles"].([]any)
	o := files[0].(map[string]any)
	if o["name"] != "photo-50.png" {
		t.Fatalf("output %v", o)
	}
	resp, _ := http.Get(m.api + "/artifacts/" + o["sha256"].(string))
	cfg, _, err := image.DecodeConfig(resp.Body)
	resp.Body.Close()
	if err != nil || cfg.Width != 50 || cfg.Height != 25 {
		t.Fatalf("resized image: %v %dx%d", err, cfg.Width, cfg.Height)
	}
}

// The phone demo: N photos resized across the fleet, zipped into one
// download — typed tasks only, no raw command anywhere.
func TestPhotoJobResizesAcrossNodesAndZips(t *testing.T) {
	const addr = "127.0.0.1:19551"
	p := domain.DefaultPolicy() // raw commands off: typed work must not need them
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, name := range []string{"photo-a", "photo-b"} {
		a := startFileAgentWithSlots(t, ctx, m, addr, name, 2) // 4 photos, 2 slots each: must spread
		waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	}
	var tasks []map[string]any
	for i, name := range []string{"beach.png", "city.png", "dog.png", "tree.png"} {
		sha := uploadArtifact(t, m.api, pngBytes(120+i, 80))
		tasks = append(tasks, map[string]any{"capability": "image.resize", "params": map[string]string{"width": "60"},
			"inputs": []map[string]string{{"name": name, "sha256": sha}}})
	}
	code, job, raw := postJob(t, m.api, map[string]any{"tasks": tasks,
		"reduce": map[string]any{"capability": "archive.zip", "params": map[string]string{"name": "small-photos.zip"}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	job = waitJob(t, m.api, job.ID, 60*time.Second)
	if job.State != domain.JobCompleted || len(job.Outputs) != 1 || job.Outputs[0].Name != "small-photos.zip" {
		t.Fatalf("job %s (%s) outputs %+v", job.State, job.Error, job.Outputs)
	}
	nodes := map[domain.NodeID]bool{}
	for _, task := range job.Tasks {
		nodes[task.Node] = true
	}
	if len(nodes) < 2 {
		t.Fatalf("expected the photos spread over both nodes, got %v", nodes)
	}
	resp, _ := http.Get(m.api + "/artifacts/" + job.Outputs[0].SHA256)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "beach-60.jpg,city-60.jpg,dog-60.jpg,tree-60.jpg" {
		t.Fatalf("zip entries %v", names)
	}
}

func TestPolicyGatesRawCommandsAndSaysHowToEnable(t *testing.T) {
	const addr = "127.0.0.1:19552"
	p := domain.DefaultPolicy()
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "gate-agent")
	cmd, args := echoArgs("raw")
	b, _ := json.Marshal(map[string]any{"command": cmd, "args": args})
	resp, _ := http.Post(m.api+"/workloads", "application/json", bytes.NewReader(b))
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(msg), "harnessctl policy type system.execute on") {
		t.Fatalf("raw command under the default policy: %d %s", resp.StatusCode, msg)
	}
	if code, _, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{{"command": cmd, "args": args}}}); code != http.StatusForbidden {
		t.Fatalf("raw job under the default policy: %d %s", code, raw)
	}
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"}); code != http.StatusAccepted {
		t.Fatalf("typed task under the default policy: %d %v", code, out)
	}
	// A misspelt type is not something policy could enable: say so.
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "image.resise"}); code != http.StatusBadRequest || !strings.Contains(out["raw"].(string), "harnessctl tasks") {
		t.Fatalf("unknown type: %d %v", code, out)
	}
	p.Types[domain.CapabilitySystemExecute] = domain.TypePolicy{Enabled: true}
	putPolicy(t, m.api, p)
	code, out := postWorkload(t, m.api, map[string]any{"command": cmd, "args": args})
	if code != http.StatusAccepted {
		t.Fatalf("raw command after opting in: %d %v", code, out)
	}
	if v := waitState(t, m.api, out["id"].(string), 15*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("raw command: %v", v)
	}
}

func TestPolicyLabelsKeepRawCommandsOnLabelledDevices(t *testing.T) {
	const addr = "127.0.0.1:19553"
	p := domain.DefaultPolicy()
	p.Types[domain.CapabilitySystemExecute] = domain.TypePolicy{Enabled: true, NodeLabels: map[string]string{"raw": "ok"}}
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := readyAgent(t, ctx, m, addr, "unlabelled")
	b := readyAgent(t, ctx, m, addr, "labelled")
	if _, err := m.srv.SetNodeMeta(b.NodeID(), domain.NodeMeta{Labels: map[string]string{"raw": "ok"}}); err != nil {
		t.Fatal(err)
	}
	cmd, args := echoArgs("where")
	for i := 0; i < 4; i++ {
		code, out := postWorkload(t, m.api, map[string]any{"command": cmd, "args": args})
		if code != http.StatusAccepted || out["target"] != string(b.NodeID()) {
			t.Fatalf("raw command %d went to %v (%d), want the labelled node", i, out["target"], code)
		}
	}
	if code, out := postWorkload(t, m.api, map[string]any{"target": a.NodeID(), "command": cmd, "args": args}); code != http.StatusConflict {
		t.Fatalf("raw command pinned to the unlabelled node: %d %v, want 409", code, out)
	}
}

func TestPolicyMaxRuntimeFailsTheAttempt(t *testing.T) {
	const addr = "127.0.0.1:19554"
	p := domain.DefaultPolicy()
	p.Types["cpu.burn"] = domain.TypePolicy{Enabled: true, MaxRuntimeSeconds: 1}
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "timeout-agent")
	code, out := postWorkload(t, m.api, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "30", "threads": "1"}})
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	start := time.Now()
	v := waitState(t, m.api, out["id"].(string), 15*time.Second)
	if v["state"] != "FAILED" || !strings.Contains(v["error"].(string), "timed out after 1s") || time.Since(start) > 10*time.Second {
		t.Fatalf("over max runtime: %v %v after %v", v["state"], v["error"], time.Since(start))
	}
}

func TestDisablingATypeStopsItsService(t *testing.T) {
	const addr = "127.0.0.1:19555"
	p := domain.DefaultPolicy()
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "service-agent")
	code, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity", "restartPolicy": "always"})
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["id"].(string)
	var v map[string]any
	waitFor(t, 15*time.Second, func() bool {
		getJSON(t, m.api+"/workloads/"+id, &v)
		n, _ := v["restartCount"].(float64)
		return n >= 1
	})
	p.Types["system.identity"] = domain.TypePolicy{Enabled: false}
	putPolicy(t, m.api, p)
	waitFor(t, 20*time.Second, func() bool { getJSON(t, m.api+"/workloads/"+id, &v); return v["state"] == "CANCELED" })
	if !strings.Contains(v["error"].(string), "policy") {
		t.Fatalf("expected the service stopped by policy, got %v", v["error"])
	}
	n := v["restartCount"]
	time.Sleep(600 * time.Millisecond)
	getJSON(t, m.api+"/workloads/"+id, &v)
	if v["state"] != "CANCELED" || v["restartCount"] != n {
		t.Fatalf("a policy-blocked service restarted again: %v", v)
	}
}

func TestDeviceCanRefuseRawCommandsItself(t *testing.T) {
	const addr = "127.0.0.1:19556"
	m := startPolicyManager(t, addr, nil) // the manager would allow them
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), "locked"), WorkDir: filepath.Join(t.TempDir(), "locked-work"),
		Name: "locked", HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", Insecure: true,
		DisabledCapabilities: []domain.CapabilityName{domain.CapabilitySystemExecute, domain.CapabilityFilesystemRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	cmd, args := echoArgs("x")
	if code, out := postWorkload(t, m.api, map[string]any{"target": a.NodeID(), "command": cmd, "args": args}); code != http.StatusConflict {
		t.Fatalf("raw command on a device that refuses them: %d %v", code, out)
	}
	code, out := postWorkload(t, m.api, map[string]any{"target": a.NodeID(), "capability": "system.identity"})
	if code != http.StatusAccepted {
		t.Fatalf("typed task: %d %v", code, out)
	}
	if v := waitState(t, m.api, out["id"].(string), 15*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("typed task on a locked-down device: %v", v)
	}
}

func TestOtherCatalogVersionIsNeverPicked(t *testing.T) {
	const addr = "127.0.0.1:19557"
	m := startPolicyManager(t, addr, nil)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "old-catalog", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: "system.identity", Version: "0"}}
	})
	defer closer.Close()
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	code, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	if code != http.StatusConflict || !strings.Contains(out["raw"].(string), "different version") {
		t.Fatalf("other catalog version: %d %v", code, out)
	}
}

func TestTypedSubmissionsAreValidated(t *testing.T) {
	const addr = "127.0.0.1:19558"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "validate-agent")
	img := uploadArtifact(t, m.api, pngBytes(10, 10))
	big1 := uploadArtifact(t, m.api, bytes.Repeat([]byte("a"), 600<<10))
	big2 := uploadArtifact(t, m.api, bytes.Repeat([]byte("b"), 600<<10))
	in := func(name, sha string) []map[string]string { return []map[string]string{{"name": name, "sha256": sha}} }
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"unknown param", map[string]any{"capability": "image.resize", "params": map[string]string{"width": "5", "dpi": "300"}, "inputs": in("a.png", img)}},
		{"out of range", map[string]any{"capability": "image.resize", "params": map[string]string{"width": "0"}, "inputs": in("a.png", img)}},
		{"wrong input type", map[string]any{"capability": "image.resize", "params": map[string]string{"width": "5"}, "inputs": in("a.txt", img)}},
		{"a command on a typed task", map[string]any{"capability": "system.identity", "command": "x"}},
		{"own outputs on a typed task", map[string]any{"capability": "system.identity", "outputs": []string{"x"}}},
		{"archive over the file limit", map[string]any{"capability": "archive.zip", "inputs": []map[string]string{{"name": "a", "sha256": big1}, {"name": "b", "sha256": big2}}}},
	} {
		if code, out := postWorkload(t, m.api, c.body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d %v, want 400", c.name, code, out)
		}
	}
	var cat struct {
		Types []struct {
			Name  string `json:"name"`
			Nodes int    `json:"nodes"`
		} `json:"types"`
	}
	getJSON(t, m.api+"/catalog", &cat)
	if len(cat.Types) < 6 {
		t.Fatalf("catalog lists %d types", len(cat.Types))
	}
	for _, ty := range cat.Types {
		if ty.Nodes != 1 {
			t.Errorf("%s offered by %d nodes, want 1", ty.Name, ty.Nodes)
		}
	}
}

func TestPolicyPersistsAndAStoredOneWins(t *testing.T) {
	const addr = "127.0.0.1:19559"
	dbPath := filepath.Join(t.TempDir(), "policy.db")
	start := func(initial *domain.Policy) (string, func()) {
		db, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		srv := manager.NewServer(ws.New(), db, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, InitialPolicy: initial})
		go srv.Run(ctx)
		api := httptest.NewServer(srv.NewHTTPHandler())
		return api.URL, func() { api.Close(); cancel(); time.Sleep(150 * time.Millisecond); db.Close() }
	}
	def := domain.DefaultPolicy()
	api, stop := start(&def)
	var got domain.Policy
	waitFor(t, 2*time.Second, func() bool { getJSON(t, api+"/policy", &got); return len(got.Types) > 0 })
	def.Types["cpu.burn"] = domain.TypePolicy{Enabled: false}
	putPolicy(t, api, def)
	stop()

	// Restarted with a different starting policy: the stored one wins.
	permissive := domain.PermissivePolicy()
	api, stop = start(&permissive)
	defer stop()
	waitFor(t, 2*time.Second, func() bool { getJSON(t, api+"/policy", &got); return len(got.Types) > 0 })
	burn, saved := got.Types["cpu.burn"]
	if !saved || burn.Enabled || got.Types[domain.CapabilitySystemExecute].Enabled || got.AllowUnlisted {
		t.Fatalf("policy after restart: %+v", got)
	}
}

// A runtime limit is only enforced by agents that advertise timeout.v1:
// an older one would run the task unbounded, so it's never picked.
func TestTimeLimitedWorkNeverGoesToAnAgentWithoutTimeouts(t *testing.T) {
	const addr = "127.0.0.1:19560"
	p := domain.DefaultPolicy()
	p.Types["cpu.burn"] = domain.TypePolicy{Enabled: true, MaxRuntimeSeconds: 60}
	m := startPolicyManager(t, addr, &p)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "no-timeouts", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: "cpu.burn", Version: "1"}, {Name: "system.identity", Version: "1"}}
	})
	defer closer.Close()
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "1"}}); code != http.StatusConflict {
		t.Fatalf("time-limited task on an agent without timeouts: %d %v", code, out)
	}
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"}); code != http.StatusAccepted {
		t.Fatalf("an unlimited task on it: %d %v", code, out)
	}
}
