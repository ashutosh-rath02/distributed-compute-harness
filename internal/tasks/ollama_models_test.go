package tasks

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

func modelEnv(t *testing.T, capability, model string) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ty, _ := catalog.Lookup(domain.CapabilityName(capability))
	canon, outputs, err := ty.Compile(map[string]string{"model": model}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	return Env{Dir: t.TempDir(), Params: canon, Outputs: outputs, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func advertisedModels(reg *Registry) string {
	for _, c := range reg.Capabilities(context.Background()) {
		if c.Name == "llm.generate" {
			return c.Attributes[catalog.AttrModels]
		}
	}
	return ""
}

// A pull shows its progress without flooding the task's capped output,
// and the new model is advertised at once (the model list isn't served
// from before the pull).
func TestPullShowsProgressAndTheModelAppearsAtOnce(t *testing.T) {
	f := &fakeOllama{models: []string{"llama3.2:latest"}}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL}) // the real 3 s model-list cache
	if got := advertisedModels(reg); got != "llama3.2:latest" {
		t.Fatalf("before: %q", got)
	}
	h, ok := reg.Lookup("llm.pull")
	if !ok {
		t.Fatal("llm.pull not registered")
	}
	env, stdout, stderr := modelEnv(t, "llm.pull", "gemma3:1b")
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{"pulling manifest\n", "pulling aaaa\n", "  5% (", " MB of 4.0 GB)\n  10% (", "  100% (4.0 GB of 4.0 GB)", "pulling bbbb\n", "  100% (10 MB of 10 MB)", "success\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress lacks %q:\n%s", want, out)
		}
	}
	// 2,050 progress reports from Ollama, a couple of KiB kept.
	if len(out) > 4<<10 || strings.Count(out, "%") > 50 {
		t.Fatalf("progress output is %d bytes, %d lines with a percentage", len(out), strings.Count(out, "%"))
	}
	if f.pulled[0] != "gemma3:1b" || !strings.Contains(stderr.String(), "pulled gemma3:1b") {
		t.Fatalf("pulled %v, %q", f.pulled, stderr.String())
	}
	if got := advertisedModels(reg); got != "gemma3:1b,llama3.2:latest" {
		t.Fatalf("right after the pull: %q", got)
	}
}

func TestPullReportsOllamasError(t *testing.T) {
	f := &fakeOllama{pullError: "pull model manifest: file does not exist"}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL})
	// Offered with no models at all: that's when a first pull matters.
	h, _ := reg.Lookup("llm.pull")
	if err := h.Available(context.Background()); err != nil {
		t.Fatalf("llm.pull with no models yet: %v", err)
	}
	env, _, _ := modelEnv(t, "llm.pull", "no-such-model")
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), "file does not exist") {
		t.Fatalf("error: %v", err)
	}
}

func TestRemoveDeletesTheListedName(t *testing.T) {
	f := &fakeOllama{models: []string{"Gemma3:1B", "llama3.2:latest"}}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL})
	if got := advertisedModels(reg); got != "gemma3:1b,llama3.2:latest" {
		t.Fatalf("before: %q", got)
	}
	h, _ := reg.Lookup("llm.remove")
	env, stdout, _ := modelEnv(t, "llm.remove", "gemma3:1b")
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "Gemma3:1B" || stdout.String() != "removed Gemma3:1B\n" {
		t.Fatalf("deleted %v, said %q", f.deleted, stdout.String())
	}
	if got := advertisedModels(reg); got != "llama3.2:latest" {
		t.Fatalf("right after removing: %q", got)
	}
	env, _, _ = modelEnv(t, "llm.remove", "gemma3:1b")
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), "isn't on this device") {
		t.Fatalf("removing it again: %v", err)
	}
}
