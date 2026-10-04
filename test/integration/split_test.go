package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

// fakeLlamaDir builds the llama.cpp stand-ins (testdata/fakellama) into a
// folder laid out like a llama.cpp release, at the given build version.
func fakeLlamaDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for name, pkg := range map[string]string{"ggml-rpc-server": "./testdata/fakellama/rpcserver", "llama-server": "./testdata/fakellama/llamaserver"} {
		out, err := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", filepath.Join(dir, name+ext), pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}
	return dir
}

func startSplitManager(t *testing.T, addr string) artifactManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second,
		ReconcileInterval: 100 * time.Millisecond, Artifacts: openArtifactStore(t, 1<<20), AIKey: testAIKey, OperatorToken: testOperatorToken})
	transport.Handle("/tunnel/", srv.TunnelHandler())
	transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return artifactManager{srv: srv, api: api.URL}
}

func startLlamaAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name, llamaDir string, use func(context.Context) domain.DeviceUse) *agent.Agent {
	t.Helper()
	if use == nil {
		use = pluggedIn
	}
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: use, LlamaCppDir: llamaDir,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", Insecure: true, OllamaURL: "127.0.0.1:1",
		CapabilityProbeInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	id := a.NodeID()
	waitFor(t, 10*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	return a
}

// operator calls the operator API with the operator token.
func operator(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type splitView struct {
	Session *struct {
		ID      string          `json:"id"`
		State   string          `json:"state"`
		Reason  string          `json:"reason"`
		Helpers []domain.NodeID `json:"helpers"`
	} `json:"session"`
}

func waitSplit(t *testing.T, api string, timeout time.Duration, want ...string) splitView {
	t.Helper()
	var v splitView
	deadline := time.Now().Add(timeout)
	for {
		_, body := operator(t, http.MethodGet, api+"/llm/split", nil)
		json.Unmarshal(body, &v)
		if v.Session != nil {
			for _, w := range want {
				if v.Session.State == w {
					return v
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("split session: %s, want %v", body, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSplitSessionRunsAModelAcrossDevices(t *testing.T) {
	const addr = "127.0.0.1:19602"
	llama := fakeLlamaDir(t, "9999")
	m := startSplitManager(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	main := startLlamaAgent(t, ctx, m, addr, "main-pc", llama, nil)
	h1 := startLlamaAgent(t, ctx, m, addr, "helper-a", llama, nil)
	h2ctx, h2cancel := context.WithCancel(ctx)
	h2 := startLlamaAgent(t, h2ctx, m, addr, "helper-b", llama, nil)
	startLlamaAgent(t, ctx, m, addr, "no-llama", "", nil)
	model := filepath.Join(t.TempDir(), "big-model.gguf")
	os.WriteFile(model, []byte("GGUF"), 0o600)

	code, body := operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "big-model", "model": model, "main": main.NodeID()})
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, body)
	}
	v := waitSplit(t, m.api, 30*time.Second, "ready", "stopped")
	if v.Session.State != "ready" || len(v.Session.Helpers) != 2 {
		t.Fatalf("session: %+v", v.Session)
	}
	// A second session while one runs: refused.
	if code, _ := operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "other", "model": model, "main": main.NodeID()}); code != http.StatusConflict {
		t.Fatalf("a second session: %d", code)
	}

	// The model is offered through /v1 and answers through every helper.
	waitFor(t, 5*time.Second, func() bool {
		resp := openAI(t, ctx, http.MethodGet, m.api+"/v1/models", nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return strings.Contains(string(b), "big-model:latest")
	})
	resp := openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("big-model", false, "hello split"))
	var whole struct {
		Choices []struct{ Message struct{ Content string } } `json:"choices"`
		Usage   struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	json.NewDecoder(resp.Body).Decode(&whole)
	resp.Body.Close()
	if len(whole.Choices) != 1 || !strings.Contains(whole.Choices[0].Message.Content, "split over 2 helpers") ||
		strings.Count(whole.Choices[0].Message.Content, "helper-") != 2 || !strings.HasSuffix(whole.Choices[0].Message.Content, "hello split") || whole.Usage.Total != 18 {
		t.Fatalf("chat: %d %+v", resp.StatusCode, whole)
	}
	resp = openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("big-model", true, "streamed"))
	var text strings.Builder
	_, done, err := readSSE(resp.Body, func(s string) { text.WriteString(s) })
	resp.Body.Close()
	if err != nil || !done || !strings.Contains(text.String(), "split over 2 helpers") {
		t.Fatalf("streamed chat: %v %v %q", err, done, text.String())
	}

	// A helper's device goes away: the session ends and says why.
	h2cancel()
	v = waitSplit(t, m.api, 15*time.Second, "stopped")
	if !strings.Contains(v.Session.Reason, "helper-b") {
		t.Fatalf("after a helper left: %+v", v.Session)
	}
	waitFor(t, 10*time.Second, func() bool {
		resp := openAI(t, ctx, http.MethodGet, m.api+"/v1/models", nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return !strings.Contains(string(b), "big-model")
	})
	// Every part ended, none left running.
	waitFor(t, 10*time.Second, func() bool {
		for _, rec := range m.srv.Workloads.List() {
			if rec.Status.State == domain.WorkloadRunning || rec.Status.State == domain.WorkloadPending {
				return false
			}
		}
		return true
	})
	_ = h1
	_ = h2
}

func TestSplitSessionEndsWhenIdleOrAHelperIsUnplugged(t *testing.T) {
	const addr = "127.0.0.1:19603"
	llama := fakeLlamaDir(t, "9999")
	other := fakeLlamaDir(t, "8888")
	m := startSplitManager(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	use, mainUse := &fakeUse{}, &fakeUse{}
	use.set(false, 0)
	mainUse.set(false, 0)
	main := startLlamaAgent(t, ctx, m, addr, "main-pc", llama, mainUse.read)
	helper := startLlamaAgent(t, ctx, m, addr, "laptop", llama, use.read)
	odd := startLlamaAgent(t, ctx, m, addr, "older-build", other, nil)
	model := filepath.Join(t.TempDir(), "m.gguf")
	os.WriteFile(model, []byte("GGUF"), 0o600)

	// A helper on a different llama.cpp build is refused up front.
	code, body := operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": model, "main": main.NodeID(), "helpers": []domain.NodeID{odd.NodeID()}})
	if code != http.StatusBadRequest || !strings.Contains(string(body), "same build") {
		t.Fatalf("mixed builds: %d %s", code, body)
	}
	// A model file the main device doesn't have: the session ends saying so.
	code, _ = operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": filepath.Join(t.TempDir(), "missing.gguf"), "main": main.NodeID(), "helpers": []domain.NodeID{helper.NodeID()}})
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	if v := waitSplit(t, m.api, 30*time.Second, "stopped"); !strings.Contains(v.Session.Reason, "isn't on this device") {
		t.Fatalf("missing model file: %+v", v.Session)
	}

	// Unplugging a helper ends a running session (and frees it).
	code, _ = operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": model, "main": main.NodeID(), "helpers": []domain.NodeID{helper.NodeID()}})
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	waitSplit(t, m.api, 30*time.Second, "ready")
	use.set(true, 0)
	if v := waitSplit(t, m.api, 15*time.Second, "stopped"); !strings.Contains(v.Session.Reason, "on battery") {
		t.Fatalf("after unplugging the helper: %+v", v.Session)
	}
	use.set(false, 0)

	// Unplugging the main device ends it too (it would hold its memory and
	// keep-awake for chats it now refuses).
	waitFor(t, 5*time.Second, func() bool {
		code, _ := operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": model, "main": main.NodeID(), "helpers": []domain.NodeID{helper.NodeID()}})
		return code == http.StatusAccepted
	})
	waitSplit(t, m.api, 30*time.Second, "ready")
	mainUse.set(true, 0)
	if v := waitSplit(t, m.api, 15*time.Second, "stopped"); !strings.Contains(v.Session.Reason, "the main device") {
		t.Fatalf("after unplugging the main device: %+v", v.Session)
	}
	mainUse.set(false, 0)

	// Nobody chats: it ends by itself.
	old := manager.SplitIdleTimeout
	manager.SplitIdleTimeout = 2 * time.Second
	defer func() { manager.SplitIdleTimeout = old }()
	waitFor(t, 5*time.Second, func() bool {
		code, _ := operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": model, "main": main.NodeID(), "helpers": []domain.NodeID{helper.NodeID()}})
		return code == http.StatusAccepted
	})
	waitSplit(t, m.api, 30*time.Second, "ready")
	if v := waitSplit(t, m.api, 15*time.Second, "stopped"); !strings.Contains(v.Session.Reason, "no chats") {
		t.Fatalf("idle: %+v", v.Session)
	}
	// And the operator can stop one.
	manager.SplitIdleTimeout = old
	operator(t, http.MethodPost, m.api+"/llm/split", map[string]any{"name": "m", "model": model, "main": main.NodeID(), "helpers": []domain.NodeID{helper.NodeID()}})
	waitSplit(t, m.api, 30*time.Second, "ready")
	if code, _ := operator(t, http.MethodDelete, m.api+"/llm/split", nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d", code)
	}
	if v := waitSplit(t, m.api, 15*time.Second, "stopped"); v.Session.Reason != "stopped by the operator" {
		t.Fatalf("stopped: %+v", v.Session)
	}
}
