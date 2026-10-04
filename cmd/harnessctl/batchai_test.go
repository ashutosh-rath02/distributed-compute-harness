package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// batchAPI is a manager API enough for the batch commands: the catalog's
// model choices, uploads, one job that finishes on its second look, and
// its report.
type batchAPI struct {
	mu      sync.Mutex
	choices map[string][]string // type -> models
	job     map[string]any
	uploads int
	looks   int
	report  string
}

func (a *batchAPI) client(t *testing.T) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/catalog":
			var types []map[string]any
			for name, models := range a.choices {
				types = append(types, map[string]any{"name": name, "choices": map[string][]string{"model": models}})
			}
			json.NewEncoder(w).Encode(map[string]any{"types": types})
		case r.Method == http.MethodPost && r.URL.Path == "/artifacts":
			io.Copy(io.Discard, r.Body)
			a.uploads++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"sha256": strings.Repeat(string(rune('0'+a.uploads)), 64)})
		case r.Method == http.MethodPost && r.URL.Path == "/jobs":
			json.NewDecoder(r.Body).Decode(&a.job)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"j1"}`)
		case r.URL.Path == "/jobs/j1":
			a.looks++
			if a.looks == 1 {
				io.WriteString(w, `{"id":"j1","state":"RUNNING","counts":{"total":1,"active":1}}`)
				return
			}
			io.WriteString(w, `{"id":"j1","state":"COMPLETED","counts":{"total":1,"completed":1},"outputs":[{"name":"labels.csv","sha256":"`+strings.Repeat("f", 64)+`"}]}`)
		case r.URL.Path == "/artifacts/"+strings.Repeat("f", 64):
			io.WriteString(w, a.report)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newAPIClient(srv.URL, "")
}

func TestClassifyRunsAJobPerFileAndSavesTheReport(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) { os.WriteFile(filepath.Join(dir, name), data, 0o600) }
	write("a.txt", []byte("an invoice"))
	write("b.txt", []byte("\xEF\xBB\xBFa receipt"))
	write("big.txt", []byte(strings.Repeat("x", 32<<10+1)))
	write("photo.txt", []byte{0xff, 0xd8, 0xff, 0xe0})
	write("empty.txt", []byte("  \n"))
	write("bad name.txt", []byte("text"))
	api := &batchAPI{choices: map[string][]string{"llm.classify": {"gemma3:1b", "gemma3:4b"}}, report: "file,label\na.txt,invoice\nb.txt,receipt\n"}
	out := t.TempDir()
	if err := cmdClassify(api.client(t), []string{filepath.Join(dir, "*.txt"), "-labels", "invoice,receipt", "-dir", out}); err != nil {
		t.Fatal(err)
	}
	tasks := api.job["tasks"].([]any)
	if len(tasks) != 2 || api.uploads != 2 {
		t.Fatalf("%d task(s), %d upload(s): only a.txt and b.txt fit a model", len(tasks), api.uploads)
	}
	first := tasks[0].(map[string]any)
	params := first["params"].(map[string]any)
	inputs := first["inputs"].([]any)
	if first["name"] != "a.txt" || first["capability"] != "llm.classify" || params["model"] != "gemma3:1b" || params["labels"] != "invoice,receipt" ||
		inputs[0].(map[string]any)["name"] != "a.txt" {
		t.Fatalf("task %v", first)
	}
	reduce := api.job["reduce"].(map[string]any)
	if reduce["capability"] != "report.collect" || reduce["params"].(map[string]any)["name"] != "labels.csv" {
		t.Fatalf("reduce %v", reduce)
	}
	if data, err := os.ReadFile(filepath.Join(out, "labels.csv")); err != nil || string(data) != api.report {
		t.Fatalf("saved report %q %v", data, err)
	}
	if err := cmdClassify(api.client(t), []string{"-labels", "spam", "-dir", t.TempDir(), filepath.Join(dir, "a.txt")}); err == nil || !strings.Contains(err.Error(), "2-32 labels") {
		t.Fatalf("one label: %v", err)
	}
}

// Summaries count llm.generate's per-file header against the limit, and
// embeddings default only to an embedding model.
func TestSummarizeAndEmbedPickTheirModelsAndLimits(t *testing.T) {
	dir := t.TempDir()
	exact := filepath.Join(dir, "exact.txt")
	os.WriteFile(exact, []byte(strings.Repeat("y", 32<<10)), 0o600)
	api := &batchAPI{choices: map[string][]string{"llm.generate": {"gemma3:1b"}}}
	// Exactly 32 KiB fits a classification, not a summary's framed context.
	if err := cmdSummarize(api.client(t), []string{exact, "-dir", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no file left") {
		t.Fatalf("a full-size file for a summary: %v", err)
	}
	if why := unfitForModel(exact, nil); why != "" {
		t.Fatalf("a full-size file for a classification: %s", why)
	}
	if err := cmdEmbed(api.client(t), []string{exact, "-dir", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no device has an embedding model") {
		t.Fatalf("embed without an embedding model: %v", err)
	}
	small := filepath.Join(dir, "small.txt")
	os.WriteFile(small, []byte("text"), 0o600)
	api.choices = map[string][]string{"llm.generate": {"gemma3:1b"}, "llm.embed": {"embeddinggemma:latest"}}
	api.report = "[]"
	if err := cmdEmbed(api.client(t), []string{small, "-dir", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	task := api.job["tasks"].([]any)[0].(map[string]any)
	if task["capability"] != "llm.embed" || task["params"].(map[string]any)["model"] != "embeddinggemma:latest" ||
		api.job["reduce"].(map[string]any)["params"].(map[string]any)["name"] != "embeddings.json" {
		t.Fatalf("embed job %v", api.job)
	}
}
