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
	"home-harness/internal/domain"
)

// batchOllama speaks /api/tags (with capabilities, as current Ollama
// lists them), /api/chat without streaming (classification) and
// /api/embed, accepting only the request shapes Ollama documents.
type batchOllama struct {
	mu sync.Mutex
	// models: name -> capabilities (nil: listed without any, as an older
	// Ollama does).
	models map[string][]string
	// answer is the chat answer's content for a user message.
	answer func(user string) string
	delay  time.Duration
	// noEmbed answers /api/embed with a bare 404, as an Ollama from before
	// it existed.
	noEmbed bool
	chats   []map[string]any
	embeds  []map[string]any
}

func (f *batchOllama) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		models, answer, delay, noEmbed := f.models, f.answer, f.delay, f.noEmbed
		f.mu.Unlock()
		has := func(name string) bool { _, ok := models[name]; return ok }
		switch r.URL.Path {
		case "/api/tags":
			var list []map[string]any
			for name, caps := range models {
				entry := map[string]any{"name": name, "size": 600000000}
				if caps != nil {
					entry["capabilities"] = caps
				}
				list = append(list, entry)
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/chat":
			var req map[string]any
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.chats = append(f.chats, req)
			f.mu.Unlock()
			model, _ := req["model"].(string)
			if !has(model) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model %q not found, try pulling it first"}`, model)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
			msgs, _ := req["messages"].([]any)
			last, _ := msgs[len(msgs)-1].(map[string]any)
			user, _ := last["content"].(string)
			// stream false: one object, the whole answer.
			json.NewEncoder(w).Encode(map[string]any{
				"model": model, "message": map[string]any{"role": "assistant", "content": answer(user)},
				"done": true, "done_reason": "stop", "prompt_eval_count": 40, "eval_count": 6,
			})
		case "/api/embed":
			if noEmbed {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "404 page not found")
				return
			}
			var req map[string]any
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.embeds = append(f.embeds, req)
			f.mu.Unlock()
			model, _ := req["model"].(string)
			input, isText := req["input"].(string)
			switch {
			case !has(model):
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model %q not found, try pulling it first"}`, model)
			case !isText:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid input type"}`)
			case req["truncate"] == false && strings.Contains(input, "LONG"):
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"the input length exceeds the context length"}`)
			default:
				// Three numbers derived from the text, as float32 values.
				json.NewEncoder(w).Encode(map[string]any{
					"model": model, "embeddings": [][]float32{{float32(len(input)), 0.25, -0.125}}, "prompt_eval_count": len(strings.Fields(input)),
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// typedEnv compiles params for a catalog type over files (name -> text,
// in order) in a fresh directory.
func typedEnv(t *testing.T, capability string, params map[string]string, files ...string) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ty, ok := catalog.Lookup(domain.CapabilityName(capability))
	if !ok {
		t.Fatalf("no type %s", capability)
	}
	dir := t.TempDir()
	var refs []string
	var inputs []domain.ArtifactRef
	for i := 0; i+1 < len(files); i += 2 {
		p := filepath.Join(dir, filepath.FromSlash(files[i]))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(files[i+1]), 0o600)
		refs = append(refs, files[i])
		inputs = append(inputs, domain.ArtifactRef{Name: files[i], SHA256: strings.Repeat("0", 64)})
	}
	canon, outputs, err := ty.Compile(params, inputs)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	return Env{Dir: dir, Params: canon, Inputs: refs, Outputs: outputs, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func advertised(reg *Registry) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range reg.Capabilities(context.Background()) {
		out[string(c.Name)] = c.Attributes
	}
	return out
}

// llm.embed lists only embedding models, the text types only the others;
// a device without an embedding model (or an Ollama that lists no
// capabilities) doesn't offer llm.embed at all.
func TestEmbedOffersOnlyEmbeddingModels(t *testing.T) {
	f := &batchOllama{models: map[string][]string{
		"gemma3:1b": {"completion"}, "embeddinggemma:latest": {"embedding"}, "Nomic-Embed-Text:v1.5": {"embedding"},
	}}
	reg := NewRegistry(Options{OllamaURL: f.server(t).URL, tagsTTL: -1})
	caps := advertised(reg)
	if got := caps["llm.embed"][catalog.AttrModels]; got != "embeddinggemma:latest,nomic-embed-text:v1.5" {
		t.Fatalf("llm.embed models %q", got)
	}
	if got := caps["llm.embed"][catalog.AttrModelSizes]; !strings.Contains(got, "embeddinggemma:latest=600000000") {
		t.Fatalf("llm.embed model sizes %q", got)
	}
	for _, text := range []string{"llm.classify", "llm.generate"} {
		if got := caps[text][catalog.AttrModels]; got != "gemma3:1b" {
			t.Fatalf("%s models %q (no embedding model may answer a prompt)", text, got)
		}
	}
	f.mu.Lock()
	f.models = map[string][]string{"gemma3:1b": {"completion"}}
	f.mu.Unlock()
	if _, ok := advertised(reg)["llm.embed"]; ok {
		t.Fatal("llm.embed offered with no embedding model")
	}
	f.mu.Lock()
	f.models = map[string][]string{"gemma3:1b": nil, "embeddinggemma:latest": nil}
	f.mu.Unlock()
	caps = advertised(reg)
	if _, ok := caps["llm.embed"]; ok {
		t.Fatal("llm.embed offered by an Ollama that doesn't say which models embed")
	}
	if got := caps["llm.classify"][catalog.AttrModels]; got != "embeddinggemma:latest,gemma3:1b" {
		t.Fatalf("llm.classify on an older Ollama: %q (every model, as llm.generate)", got)
	}
	f.mu.Lock()
	f.models = map[string][]string{"embeddinggemma:latest": {"embedding"}}
	f.mu.Unlock()
	if _, ok := advertised(reg)["llm.classify"]; ok {
		t.Fatal("llm.classify offered with only an embedding model")
	}
}

func TestEmbedWritesOneEntryPerFile(t *testing.T) {
	f := &batchOllama{models: map[string][]string{"EmbeddingGemma:latest": {"embedding"}}}
	h, _ := NewRegistry(Options{OllamaURL: f.server(t).URL}).Lookup("llm.embed")
	env, stdout, _ := typedEnv(t, "llm.embed", map[string]string{"model": "embeddinggemma"}, "a.txt", "\xEF\xBB\xBFfirst file", "b.md", "the second one")
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var got []Embedding
	data, _ := os.ReadFile(env.out(0))
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("embeddings.json: %v\n%s", err, data)
	}
	if len(got) != 2 || got[0].Name != "a.txt" || got[1].Name != "b.md" || got[0].Model != "embeddinggemma:latest" ||
		got[0].Vector[0] != float32(len("first file")) || got[1].Vector[2] != -0.125 {
		t.Fatalf("entries %+v", got)
	}
	if !strings.Contains(string(data), `"vector":[10,0.25,-0.125]`) {
		t.Fatalf("vectors not written as given: %s", data)
	}
	for _, req := range f.embeds {
		// The name exactly as Ollama listed it; the text without its BOM;
		// never cut to fit.
		if req["model"] != "EmbeddingGemma:latest" || req["truncate"] != false {
			t.Fatalf("embed request %v", req)
		}
	}
	if f.embeds[0]["input"] != "first file" {
		t.Fatalf("input %q", f.embeds[0]["input"])
	}
	if !strings.Contains(stdout.String(), "a.txt: 3 numbers") || !strings.Contains(stdout.String(), "b.md: 3 numbers") {
		t.Fatalf("progress %q", stdout.String())
	}
}

func TestEmbedRefusesWhatAModelCantTake(t *testing.T) {
	f := &batchOllama{models: map[string][]string{"embeddinggemma:latest": {"embedding"}}}
	h, _ := NewRegistry(Options{OllamaURL: f.server(t).URL}).Lookup("llm.embed")
	for name, c := range map[string]struct{ text, want string }{
		"too big":       {strings.Repeat("x", MaxContextBytes+1), "over 32 KiB"},
		"not text":      {"\xff\xfeh\x00i\x00", "isn't UTF-8 text"},
		"empty":         {" \n\t", "is empty"},
		"over context":  {"a LONG text", "a.txt: Ollama: the input length exceeds the context length"},
		"just in limit": {strings.Repeat("y", MaxContextBytes), ""},
	} {
		env, _, _ := typedEnv(t, "llm.embed", map[string]string{"model": "embeddinggemma"}, "a.txt", c.text)
		err := h.Run(context.Background(), env)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	f.mu.Lock()
	f.noEmbed = true
	f.mu.Unlock()
	env, _, _ := typedEnv(t, "llm.embed", map[string]string{"model": "embeddinggemma"}, "a.txt", "hello")
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), "update Ollama") {
		t.Fatalf("an Ollama without /api/embed: %v", err)
	}
}

// The answer is constrained to the labels (a JSON schema whose only
// values are them) and checked again.
func TestClassifyAnswersOnlyWithOneOfTheLabels(t *testing.T) {
	f := &batchOllama{models: map[string][]string{"Gemma3:1b": {"completion"}}, answer: func(user string) string {
		if strings.Contains(user, "Total due") {
			return `{"label": "invoice"}`
		}
		return `{"label":"letter"}`
	}}
	h, _ := NewRegistry(Options{OllamaURL: f.server(t).URL}).Lookup("llm.classify")
	env, stdout, stderr := typedEnv(t, "llm.classify", map[string]string{"model": "gemma3:1b", "labels": "invoice, receipt ,letter"}, "bill.txt", "Total due: 40 EUR")
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(env.out(0))
	if string(data) != `{"file":"bill.txt","label":"invoice"}`+"\n" {
		t.Fatalf("label.json %q", data)
	}
	if stdout.String() != "bill.txt: invoice\n" || !strings.Contains(stderr.String(), "40 prompt tokens") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	req := f.chats[0]
	format, _ := json.Marshal(req["format"])
	if string(format) != `{"properties":{"label":{"enum":["invoice","receipt","letter"],"type":"string"}},"required":["label"],"type":"object"}` {
		t.Fatalf("format %s", format)
	}
	options, _ := req["options"].(map[string]any)
	if req["model"] != "Gemma3:1b" || req["stream"] != false || options["temperature"] != float64(0) {
		t.Fatalf("chat request %v", req)
	}
	user := req["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "Labels: invoice, receipt, letter") || !strings.Contains(user, "Total due: 40 EUR") || !strings.Contains(user, `"bill.txt"`) {
		t.Fatalf("user message %q", user)
	}
	for answer, want := range map[string]string{
		`{"label":"spam"}`:      "isn't one of the labels",
		`The label is invoice.`: "didn't answer with a label",
		`{"category":"letter"}`: "didn't answer with a label",
		`{"label":" LETTER "}`:  "",
	} {
		f.mu.Lock()
		f.answer = func(string) string { return answer }
		f.mu.Unlock()
		env, _, _ := typedEnv(t, "llm.classify", map[string]string{"model": "gemma3:1b", "labels": "invoice,receipt,letter"}, "x.txt", "text")
		err := h.Run(context.Background(), env)
		if want == "" {
			data, _ := os.ReadFile(env.out(0))
			if err != nil || !strings.Contains(string(data), `"label":"letter"`) {
				t.Errorf("%s: %v %s (want the label as the operator wrote it)", answer, err, data)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", answer, err, want)
		}
		if _, statErr := os.Stat(env.out(0)); statErr == nil {
			t.Errorf("%s: label.json written for a refused answer", answer)
		}
	}
}

func TestClassifyHonorsCancelAndAMissingModel(t *testing.T) {
	f := &batchOllama{models: map[string][]string{"gemma3:1b": {"completion"}}, delay: 3 * time.Second, answer: func(string) string { return `{"label":"a"}` }}
	h, _ := NewRegistry(Options{OllamaURL: f.server(t).URL}).Lookup("llm.classify")
	env, _, _ := typedEnv(t, "llm.classify", map[string]string{"model": "gemma3:1b", "labels": "a,b"}, "x.txt", "text")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.Run(ctx, env); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(start))
	}
	env, _, _ = typedEnv(t, "llm.classify", map[string]string{"model": "llama3.2", "labels": "a,b"}, "x.txt", "text")
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), `"llama3.2" isn't on this device`) {
		t.Fatalf("missing model: %v", err)
	}
}
