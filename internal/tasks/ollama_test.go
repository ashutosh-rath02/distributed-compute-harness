package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/catalog"
)

// fakeOllama speaks the two endpoints the adapter uses.
type fakeOllama struct {
	mu      sync.Mutex
	models  []string
	chunks  []string
	delay   time.Duration
	prompts []string
	sent    []string // the model names generate requests named
	chats   []fakeChat
	reason  string // done_reason of the last chat chunk ("" = stop)
	// Model management: what pulls and deletes named, and an error for
	// the next pull to end with.
	pulled, deleted []string
	pullError       string
}

// fakeChat is one /api/chat request as the fake received it.
type fakeChat struct {
	Model    string
	Messages []ChatMessage
	Options  map[string]any
}

func (f *fakeOllama) setModels(m ...string) { f.mu.Lock(); f.models = m; f.mu.Unlock() }

func (f *fakeOllama) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		models := append([]string(nil), f.models...)
		chunks := append([]string(nil), f.chunks...)
		delay := f.delay
		reason := f.reason
		f.mu.Unlock()
		if reason == "" {
			reason = "stop"
		}
		switch r.URL.Path {
		case "/api/tags":
			var list []map[string]any
			for _, m := range models {
				list = append(list, map[string]any{"name": m, "size": 2000000000, "details": map[string]any{"parameter_size": "3B", "quantization_level": "Q4_K_M"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/generate":
			var req struct {
				Model, Prompt string
				Stream        bool
			}
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.prompts = append(f.prompts, req.Prompt)
			f.sent = append(f.sent, req.Model)
			f.mu.Unlock()
			known := false
			for _, m := range models {
				known = known || catalog.NormalizeModel(m) == catalog.NormalizeModel(req.Model)
			}
			if !known {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model \"%s\" not found, try pulling it first"}`, req.Model)
				return
			}
			fl := w.(http.Flusher)
			for _, c := range chunks {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
				b, _ := json.Marshal(map[string]any{"response": c, "done": false})
				w.Write(append(b, '\n'))
				fl.Flush()
			}
			w.Write([]byte(`{"response":"","done":true,"eval_count":7,"eval_duration":1000000000,"done_reason":"stop"}` + "\n"))
		case "/api/pull":
			// Shapes from Ollama's API doc: POST {"model", "stream"}; a
			// stream of {"status", "digest", "total", "completed"} ending
			// with {"status":"success"}, or an {"error"} line.
			var req struct {
				Model  string `json:"model"`
				Stream *bool  `json:"stream"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || req.Model == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"model is required"}`)
				return
			}
			f.mu.Lock()
			f.pulled = append(f.pulled, req.Model)
			fail := f.pullError
			f.mu.Unlock()
			fl := w.(http.Flusher)
			line := func(v map[string]any) { b, _ := json.Marshal(v); w.Write(append(b, '\n')); fl.Flush() }
			line(map[string]any{"status": "pulling manifest"})
			if fail != "" {
				line(map[string]any{"error": fail})
				return
			}
			// A 4 GB layer reported in 1000 small steps, then a small one.
			for _, layer := range []struct {
				digest string
				total  int64
				steps  int
			}{{"sha256:aaaa", 4 << 30, 1000}, {"sha256:bbbb", 10 << 20, 50}} {
				for i := 0; i <= layer.steps; i++ {
					line(map[string]any{"status": "pulling " + layer.digest[7:], "digest": layer.digest, "total": layer.total, "completed": layer.total * int64(i) / int64(layer.steps)})
				}
			}
			line(map[string]any{"status": "verifying sha256 digest"})
			line(map[string]any{"status": "writing manifest"})
			f.mu.Lock()
			f.models = append(f.models, req.Model)
			f.mu.Unlock()
			line(map[string]any{"status": "success"})
		case "/api/delete":
			var req struct {
				Model string `json:"model"`
			}
			if r.Method != http.MethodDelete || json.NewDecoder(r.Body).Decode(&req) != nil || req.Model == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deleted = append(f.deleted, req.Model)
			for i, m := range f.models {
				if m == req.Model {
					f.models = append(f.models[:i], f.models[i+1:]...)
					return // 200, no body
				}
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":"model '%s' not found"}`, req.Model)
		case "/api/chat":
			var req fakeChat
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.chats = append(f.chats, req)
			f.mu.Unlock()
			known := false
			for _, m := range models {
				known = known || m == req.Model
			}
			if !known {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model \"%s\" not found, try pulling it first"}`, req.Model)
				return
			}
			fl := w.(http.Flusher)
			for _, c := range chunks {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
				b, _ := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": c}, "done": false})
				w.Write(append(b, '\n'))
				fl.Flush()
			}
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":12,"eval_count":7,"eval_duration":1000000000,"done_reason":%q}`+"\n", reason)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func generateEnv(t *testing.T, params map[string]string, files map[string]string, order ...string) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ty, _ := catalog.Lookup("llm.generate")
	var refs []string
	dir := t.TempDir()
	for _, n := range order {
		os.WriteFile(filepath.Join(dir, n), []byte(files[n]), 0o600)
		refs = append(refs, n)
	}
	canon, outputs, err := ty.Compile(params, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	return Env{Dir: dir, Params: canon, Inputs: refs, Outputs: outputs, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestGenerateStreamsTheAnswerAndSavesIt(t *testing.T) {
	f := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"The sky ", "is blue ", "because..."}}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL})
	h, _ := reg.Lookup("llm.generate")
	env, stdout, stderr := generateEnv(t, map[string]string{"model": "llama3.2", "prompt": "why?"}, nil)
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env.out(0))
	if stdout.String() != "The sky is blue because..." || string(b) != stdout.String() {
		t.Fatalf("stdout %q, response.txt %q", stdout.String(), b)
	}
	if !strings.Contains(stderr.String(), "7 tokens, 7.0 tokens/s") {
		t.Fatalf("stats %q", stderr.String())
	}
}

func TestGenerateIncludesContextFilesAndCapsThem(t *testing.T) {
	f := &fakeOllama{models: []string{"m:latest"}, chunks: []string{"ok"}}
	srv := f.server(t)
	h, _ := NewRegistry(Options{OllamaURL: srv.URL}).Lookup("llm.generate")
	env, _, _ := generateEnv(t, map[string]string{"model": "m", "prompt": "summarize"}, map[string]string{"a.txt": "alpha", "b.md": "beta"}, "a.txt", "b.md")
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := f.prompts[0]; got != "--- a.txt ---\nalpha\n\n--- b.md ---\nbeta\n\nsummarize" {
		t.Fatalf("prompt sent %q", got)
	}
	env, _, _ = generateEnv(t, map[string]string{"model": "m", "prompt": "x"}, map[string]string{"big.txt": strings.Repeat("z", MaxContextBytes)}, "big.txt")
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), "KiB") {
		t.Fatalf("oversized context: %v", err)
	}
}

func TestGenerateReportsAMissingModelAndHonorsCancel(t *testing.T) {
	f := &fakeOllama{models: []string{"m:latest"}, chunks: []string{"a", "b", "c", "d", "e"}, delay: 300 * time.Millisecond}
	srv := f.server(t)
	h, _ := NewRegistry(Options{OllamaURL: srv.URL}).Lookup("llm.generate")
	env, _, _ := generateEnv(t, map[string]string{"model": "nope", "prompt": "x"}, nil)
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), `"nope" isn't on this device`) {
		t.Fatalf("missing model: %v", err)
	}
	env, stdout, _ := generateEnv(t, map[string]string{"model": "m", "prompt": "x"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.Run(ctx, env)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second || stdout.String() != "a" {
		t.Fatalf("cancel mid-stream: %v after %v, got %q", err, time.Since(start), stdout.String())
	}
}

func TestOllamaAvailabilityAndModelAttributes(t *testing.T) {
	f := &fakeOllama{}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL, tagsTTL: -1})
	names := func() []string {
		var out []string
		for _, c := range reg.Capabilities(context.Background()) {
			out = append(out, string(c.Name))
		}
		return out
	}
	// Ollama without models: only a download can do anything.
	if got := strings.Join(names(), ","); !strings.Contains(got, "llm.pull") || strings.Contains(strings.ReplaceAll(got, "llm.pull", ""), "llm.") {
		t.Fatalf("with no models: %s (want llm.pull only)", got)
	}
	f.setModels("Qwen2.5:7B", "llama3.2", "llama3.2:latest")
	var gen map[string]string
	for _, c := range reg.Capabilities(context.Background()) {
		if c.Name == "llm.generate" {
			gen = c.Attributes
		}
	}
	if gen[catalog.AttrModels] != "llama3.2:latest,qwen2.5:7b" {
		t.Fatalf("models attribute %q (want normalized, deduplicated, sorted)", gen[catalog.AttrModels])
	}
	if gen[catalog.AttrModelSizes] != "llama3.2:latest=2000000000,qwen2.5:7b=2000000000" {
		t.Fatalf("model sizes %q", gen[catalog.AttrModelSizes])
	}
	if !strings.Contains(strings.Join(names(), ","), "llm.inventory") {
		t.Fatal("llm.inventory not advertised")
	}
	inv, _ := reg.Lookup("llm.inventory")
	var out bytes.Buffer
	if err := inv.Run(context.Background(), Env{Stdout: &out}); err != nil || !strings.Contains(out.String(), `"parameterSize": "3B"`) {
		t.Fatalf("inventory: %v %s", err, out.String())
	}
	srv.Close()
	if strings.Contains(strings.Join(names(), ","), "llm.") {
		t.Fatal("llm types still advertised after Ollama went away")
	}
}

func TestNormalizeOllamaURL(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	for in, want := range map[string]string{
		"":                       "http://127.0.0.1:11434",
		"0.0.0.0":                "http://127.0.0.1:11434",
		"0.0.0.0:11434":          "http://127.0.0.1:11434",
		"192.168.1.5":            "http://192.168.1.5:11434",
		"http://mac.local:8080/": "http://mac.local:8080",
		"https://gpu.lan":        "https://gpu.lan:11434",
	} {
		if got := NormalizeOllamaURL(in); got != want {
			t.Errorf("NormalizeOllamaURL(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("OLLAMA_HOST", "0.0.0.0:9999")
	if got := NormalizeOllamaURL(""); got != "http://127.0.0.1:9999" {
		t.Fatalf("from OLLAMA_HOST: %q", got)
	}
}

// Placement matches normalized names; Ollama must get the name exactly
// as it listed it.
func TestGenerateSendsTheListedModelName(t *testing.T) {
	f := &fakeOllama{models: []string{"HF.co/Org/Repo:Q4"}, chunks: []string{"ok"}}
	srv := f.server(t)
	h, _ := NewRegistry(Options{OllamaURL: srv.URL}).Lookup("llm.generate")
	env, _, _ := generateEnv(t, map[string]string{"model": "hf.co/org/repo:q4", "prompt": "x"}, nil)
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "HF.co/Org/Repo:Q4" {
		t.Fatalf("sent %v, want the listed name", f.sent)
	}
}
