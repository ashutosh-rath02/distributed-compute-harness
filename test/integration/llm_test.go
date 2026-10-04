package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// fakeOllama answers /api/tags and streams /api/generate, slowly enough
// for progress reports to be seen.
type fakeOllama struct {
	mu     sync.Mutex
	models []string
	chunks []string
	delay  time.Duration
	active int
	peak   int
	// Chat only: the last user message received, and answers cut off by
	// the caller going away.
	lastUser string
	cut      int
}

func (f *fakeOllama) set(models ...string) { f.mu.Lock(); f.models = models; f.mu.Unlock() }

func startFakeOllama(t *testing.T, f *fakeOllama) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		models, chunks, delay := append([]string(nil), f.models...), append([]string(nil), f.chunks...), f.delay
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/tags":
			var list []map[string]any
			for _, m := range models {
				list = append(list, map[string]any{"name": m, "size": 2_000_000_000})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/pull":
			var req struct{ Model string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Model == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"status":"pulling manifest"}` + "\n" + `{"status":"pulling 1234","total":100,"completed":100}` + "\n"))
			f.mu.Lock()
			f.models = append(f.models, req.Model)
			f.mu.Unlock()
			w.Write([]byte(`{"status":"success"}` + "\n"))
		case "/api/delete":
			var req struct{ Model string }
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			defer f.mu.Unlock()
			for i, m := range f.models {
				if m == req.Model && r.Method == http.MethodDelete {
					f.models = append(f.models[:i], f.models[i+1:]...)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case "/api/generate":
			var req struct{ Model string }
			json.NewDecoder(r.Body).Decode(&req)
			found := false
			for _, m := range models {
				found = found || strings.HasPrefix(m, strings.SplitN(req.Model, ":", 2)[0])
			}
			if !found {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model %q not found"}`, req.Model)
				return
			}
			f.mu.Lock()
			f.active++
			if f.active > f.peak {
				f.peak = f.active
			}
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
			for _, c := range chunks {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
				b, _ := json.Marshal(map[string]any{"response": c})
				w.Write(append(b, '\n'))
				w.(http.Flusher).Flush()
			}
			w.Write([]byte(`{"done":true,"eval_count":3,"eval_duration":1000000000,"done_reason":"stop"}` + "\n"))
		case "/api/chat":
			var req struct {
				Model    string
				Messages []struct{ Role, Content string }
			}
			json.NewDecoder(r.Body).Decode(&req)
			found := false
			for _, m := range models {
				found = found || m == req.Model
			}
			if !found {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model %q not found"}`, req.Model)
				return
			}
			f.mu.Lock()
			f.active++
			if f.active > f.peak {
				f.peak = f.active
			}
			if len(req.Messages) > 0 {
				f.lastUser = req.Messages[len(req.Messages)-1].Content
			}
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
			for _, c := range chunks {
				select {
				case <-r.Context().Done():
					f.mu.Lock()
					f.cut++
					f.mu.Unlock()
					return
				case <-time.After(delay):
				}
				b, _ := json.Marshal(map[string]any{"message": map[string]string{"role": "assistant", "content": c}})
				w.Write(append(b, '\n'))
				w.(http.Flusher).Flush()
			}
			w.Write([]byte(`{"done":true,"prompt_eval_count":11,"eval_count":3,"eval_duration":1000000000,"done_reason":"stop"}` + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func startLLMAgent(t *testing.T, ctx context.Context, addr, name, ollamaURL string, slots int) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
		HostFingerprint: "-", Insecure: true, WorkloadSlots: slots, OllamaURL: ollamaURL, CapabilityProbeInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	return a
}

func ask(model, prompt string) map[string]any {
	return map[string]any{"capability": "llm.generate", "params": map[string]string{"model": model, "prompt": prompt}}
}

func TestLLMTypesOnlyWhereOllamaAnswersAndPlacedByModel(t *testing.T) {
	const addr = "127.0.0.1:19570"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	llama := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"from ", "llama"}}
	qwen := &fakeOllama{models: []string{"qwen2.5:7b"}, chunks: []string{"from ", "qwen"}}
	a := startLLMAgent(t, ctx, addr, "llama-box", startFakeOllama(t, llama), 2)
	b := startLLMAgent(t, ctx, addr, "qwen-box", startFakeOllama(t, qwen), 2)
	c := startLLMAgent(t, ctx, addr, "no-ollama", "127.0.0.1:1", 2)
	for _, ag := range []*agent.Agent{a, b, c} {
		id := ag.NodeID()
		waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	}
	var models []struct {
		Model string `json:"model"`
		Nodes []struct {
			ID domain.NodeID `json:"id"`
		} `json:"nodes"`
	}
	getJSON(t, m.api+"/models", &models)
	if len(models) != 2 || models[0].Model != "llama3.2:latest" || models[0].Nodes[0].ID != a.NodeID() || models[1].Model != "qwen2.5:7b" {
		t.Fatalf("GET /models = %+v", models)
	}
	var cat struct {
		Types []struct {
			Name    string              `json:"name"`
			Nodes   int                 `json:"nodes"`
			Choices map[string][]string `json:"choices"`
		} `json:"types"`
	}
	getJSON(t, m.api+"/catalog", &cat)
	for _, ty := range cat.Types {
		if ty.Name == "llm.generate" && (ty.Nodes != 2 || strings.Join(ty.Choices["model"], ",") != "llama3.2:latest,qwen2.5:7b") {
			t.Fatalf("llm.generate in the catalog: %+v", ty)
		}
	}
	for model, want := range map[string]*agent.Agent{"qwen2.5:7b": b, "llama3.2": a} {
		code, out := postWorkload(t, m.api, ask(model, "hi"))
		if code != http.StatusAccepted {
			t.Fatalf("%s: %d %v", model, code, out)
		}
		// (It may queue for a moment until the node's first heartbeat
		// reports its free memory; where it finally runs is what counts.)
		v := waitState(t, m.api, out["id"].(string), 15*time.Second)
		if v["state"] != "COMPLETED" || v["target"] != string(want.NodeID()) || !strings.HasPrefix(v["stdout"].(string), "from ") {
			t.Fatalf("%s: ran on %v, want %s: %v", model, v["target"], want.NodeID(), v)
		}
	}
	code, out := postWorkload(t, m.api, ask("mistral", "hi"))
	if code != http.StatusConflict || !strings.Contains(out["raw"].(string), `has no model "mistral"`) {
		t.Fatalf("a model no device has: %d %v", code, out)
	}
}

// The answer streams: progress reports carry the text so far before the
// workload completes, and the saved response matches the whole answer.
func TestGenerationStreamsProgressAndKeepsOnePerNode(t *testing.T) {
	const addr = "127.0.0.1:19571"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"one ", "two ", "three ", "four ", "five"}, delay: 500 * time.Millisecond}
	a := startLLMAgent(t, ctx, addr, "streamer", startFakeOllama(t, f), 4)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	_, first := postWorkload(t, m.api, ask("llama3.2", "count"))
	code, second := postWorkload(t, m.api, ask("llama3.2", "count again"))
	if code != http.StatusAccepted || second["state"] != "QUEUED" {
		t.Fatalf("a second generation on a node with 4 slots but one model runtime: %d %v, want QUEUED", code, second)
	}
	id := first["id"].(string)
	var partial string
	waitFor(t, 10*time.Second, func() bool {
		var v map[string]any
		getJSON(t, m.api+"/workloads/"+id, &v)
		s, _ := v["stdout"].(string)
		if v["state"] == "RUNNING" && s != "" && s != "one two three four five" {
			partial = s
			return true
		}
		return false
	})
	v := waitState(t, m.api, id, 15*time.Second)
	if v["state"] != "COMPLETED" || v["stdout"] != "one two three four five" || !strings.HasPrefix(v["stdout"].(string), partial) {
		t.Fatalf("final %v (partial was %q)", v, partial)
	}
	out := v["outputFiles"].([]any)[0].(map[string]any)
	resp, _ := http.Get(m.api + "/artifacts/" + out["sha256"].(string))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if out["name"] != "response.txt" || string(body) != "one two three four five" {
		t.Fatalf("response.txt %v %q", out, body)
	}
	if v := waitState(t, m.api, second["id"].(string), 20*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("queued generation: %v", v)
	}
	if f.peak != 1 {
		t.Fatalf("the model runtime saw %d generations at once, want 1", f.peak)
	}
	rec, _ := m.srv.Workloads.Get(domain.WorkloadID(id))
	if rec.Workload.TimeoutSeconds != 600 {
		t.Fatalf("llm.generate default max runtime: %d, want 600", rec.Workload.TimeoutSeconds)
	}
}

func TestModelPulledLaterIsPickedUpAndCancelStopsAnAnswer(t *testing.T) {
	const addr = "127.0.0.1:19572"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := &fakeOllama{chunks: []string{"a", "b", "c", "d", "e", "f"}, delay: 700 * time.Millisecond}
	a := startLLMAgent(t, ctx, addr, "pull-later", startFakeOllama(t, f), 2)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	if code, _ := postWorkload(t, m.api, ask("llama3.2", "x")); code != http.StatusConflict {
		t.Fatalf("before any model: %d, want 409", code)
	}
	f.set("llama3.2:latest") // "ollama pull" on the device
	var code int
	var out map[string]any
	waitFor(t, 5*time.Second, func() bool {
		code, out = postWorkload(t, m.api, ask("llama3.2", "x"))
		return code == http.StatusAccepted
	})
	id := out["id"].(string)
	waitFor(t, 10*time.Second, func() bool {
		var v map[string]any
		getJSON(t, m.api+"/workloads/"+id, &v)
		return v["state"] == "RUNNING"
	})
	start := time.Now()
	if err := m.srv.CancelWorkload(ctx, domain.WorkloadID(id)); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, m.api, id, 10*time.Second); v["state"] != "CANCELED" || time.Since(start) > 5*time.Second {
		t.Fatalf("cancel mid-answer: %v after %v", v["state"], time.Since(start))
	}
}

// A progress report that arrives after the final one must not bring a
// finished workload back to RUNNING (it would hold its slot forever).
func TestLateProgressCannotReviveAFinishedWorkload(t *testing.T) {
	const addr = "127.0.0.1:19573"
	m := startPolicyManager(t, addr, nil)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "late-reporter", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		for {
			if _, err := conn.Receive(ctx); err != nil {
				return
			}
		}
	}()
	report := func(id string, st domain.WorkloadState) {
		env, _ := protocol.NewEnvelope(protocol.MsgWorkloadStatus, nodeID, domain.ManagerNodeID, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{
			ID: domain.WorkloadID(id), Target: nodeID, State: st, StartedAt: time.Now().UTC(), Stdout: "late",
		}})
		wire, _ := protocol.Encode(env)
		conn.Send(ctx, wire)
	}
	_, out := postWorkload(t, m.api, map[string]any{"target": nodeID, "command": "x"})
	id := out["id"].(string)
	report(id, domain.WorkloadRunning)
	report(id, domain.WorkloadCompleted)
	report(id, domain.WorkloadRunning)
	time.Sleep(300 * time.Millisecond)
	if rec, _ := m.srv.Workloads.Get(domain.WorkloadID(id)); rec.Status.State != domain.WorkloadCompleted {
		t.Fatalf("a late RUNNING revived a finished workload: %s", rec.Status.State)
	}
	if code, next := postWorkload(t, m.api, map[string]any{"target": nodeID, "command": "y"}); code != http.StatusAccepted || next["state"] == "QUEUED" {
		t.Fatalf("the node's only slot is still held: %d %v", code, next)
	}
}
