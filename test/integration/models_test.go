package integration

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/transport/ws"
)

// modelsOn lists the models GET /models says node has.
func modelsOn(t *testing.T, api string, node domain.NodeID) string {
	t.Helper()
	var rows []struct {
		Model string `json:"model"`
		Nodes []struct {
			ID domain.NodeID `json:"id"`
		} `json:"nodes"`
	}
	getJSON(t, api+"/models", &rows)
	var out []string
	for _, r := range rows {
		for _, n := range r.Nodes {
			if n.ID == node {
				out = append(out, r.Model)
			}
		}
	}
	return strings.Join(out, ",")
}

// Downloading and removing a model on one device from the manager: the
// new list reaches the manager right away (the agent's regular check is a
// minute apart here), and the task must say which device.
func TestModelsAreDownloadedAndRemovedPerDevice(t *testing.T) {
	const addr = "127.0.0.1:19598"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"hi"}}
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), "ai-box"), WorkDir: filepath.Join(t.TempDir(), "ai-box-work"),
		Name: "ai-box", HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", Insecure: true, WorkloadSlots: 2,
		OllamaURL: startFakeOllama(t, f), CapabilityProbeInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	id := a.NodeID()
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	if got := modelsOn(t, m.api, id); got != "llama3.2:latest" {
		t.Fatalf("before: %q", got)
	}

	code, out := postWorkload(t, m.api, map[string]any{"capability": "llm.pull", "params": map[string]string{"model": "gemma3:1b"}})
	if code != http.StatusBadRequest || !strings.Contains(out["raw"].(string), "say which device") {
		t.Fatalf("a pull without a device: %d %v", code, out)
	}
	_, out = postWorkload(t, m.api, map[string]any{"capability": "llm.pull", "target": id, "params": map[string]string{"model": "gemma3:1b"}})
	v := waitState(t, m.api, out["id"].(string), 10*time.Second)
	if v["state"] != "COMPLETED" || !strings.Contains(v["stdout"].(string), "success") {
		t.Fatalf("pull: %v %v", v["state"], v["stdout"])
	}
	done := time.Now()
	waitFor(t, 5*time.Second, func() bool { return modelsOn(t, m.api, id) == "gemma3:1b,llama3.2:latest" })
	t.Logf("new model on the manager %v after the pull finished", time.Since(done).Round(10*time.Millisecond))

	// The new model is usable straight away.
	_, out = postWorkload(t, m.api, ask("gemma3:1b", "hello"))
	if v := waitState(t, m.api, out["id"].(string), 10*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("asking the new model: %v %v", v["state"], v["error"])
	}

	_, out = postWorkload(t, m.api, map[string]any{"capability": "llm.remove", "target": id, "params": map[string]string{"model": "gemma3:1b"}})
	if v := waitState(t, m.api, out["id"].(string), 10*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("remove: %v %v", v["state"], v["error"])
	}
	waitFor(t, 5*time.Second, func() bool { return modelsOn(t, m.api, id) == "llama3.2:latest" })
}

// Two devices have the model; every request goes to the one whose GPU
// fits it, end to end (the unit test TestPlacementPrefersTheGPUThatFits-
// OverFreeMemory pins down that the GPU wins over free memory).
func TestAIWorkPrefersTheDeviceWhoseGPUFitsTheModel(t *testing.T) {
	const addr = "127.0.0.1:19599"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gpuFor := map[string]bool{}
	gpus := func(name string) func(context.Context) []domain.GPU {
		return func(context.Context) []domain.GPU {
			if gpuFor[name] {
				return []domain.GPU{{Name: "NVIDIA GeForce RTX 3060", Vendor: "nvidia", MemoryBytes: 12 << 30}}
			}
			return []domain.GPU{{Name: "Intel Iris Xe", Vendor: "intel", MemoryBytes: 16 << 30, Integrated: true}}
		}
	}
	var agents []*agent.Agent
	for _, name := range []string{"box-a", "box-b"} {
		a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, GPUs: gpus(name),
			ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
			Name: name, HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", Insecure: true, WorkloadSlots: 2,
			OllamaURL: startFakeOllama(t, &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"ok"}}), CapabilityProbeInterval: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	// Put the GPU on the one a pure node-ID tie-break wouldn't pick.
	gpuAgent := agents[0]
	if agents[1].NodeID() > agents[0].NodeID() {
		gpuAgent = agents[1]
	}
	gpuFor[map[*agent.Agent]string{agents[0]: "box-a", agents[1]: "box-b"}[gpuAgent]] = true
	for _, a := range agents {
		go a.Run(ctx)
	}
	for _, a := range agents {
		id := a.NodeID()
		waitFor(t, 5*time.Second, func() bool {
			rec, ok := m.srv.Registry.Get(id)
			return ok && rec.State == domain.NodeReady && rec.HasCapability("llm.generate") && !rec.LastMetrics.LastHeartbeat.IsZero()
		})
	}
	var n struct {
		GPUs []domain.GPU `json:"gpus"`
	}
	getJSON(t, m.api+"/nodes/"+string(gpuAgent.NodeID()), &n)
	if len(n.GPUs) != 1 || n.GPUs[0].MemoryBytes != 12<<30 {
		t.Fatalf("GPUs on the node view: %+v", n.GPUs)
	}
	for i := 0; i < 3; i++ {
		_, out := postWorkload(t, m.api, ask("llama3.2", "which box?"))
		v := waitState(t, m.api, out["id"].(string), 10*time.Second)
		if v["state"] != "COMPLETED" || v["target"] != string(gpuAgent.NodeID()) {
			t.Fatalf("run %d: %v on %v, want the GPU box %s", i, v["state"], v["target"], gpuAgent.NodeID())
		}
	}
	// Other work isn't steered by GPUs.
	_, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	if v := waitState(t, m.api, out["id"].(string), 10*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("plain task: %v", v["state"])
	}
}
