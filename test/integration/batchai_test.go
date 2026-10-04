package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
)

// Batch AI over a folder (summarise, classify, embed every file) against
// a fake Ollama per device that answers from what it was sent, so the
// report shows which device's answer landed under which file.
type batchAIOllama struct {
	mu     sync.Mutex
	models map[string][]string // name -> capabilities
	delay  time.Duration
	served int
}

var docHeader = regexp.MustCompile(`--- (.+) ---`)

func startBatchAIOllama(t *testing.T, f *batchAIOllama) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		models, delay := f.models, f.delay
		f.mu.Unlock()
		var req map[string]any
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&req)
		}
		model, _ := req["model"].(string)
		if r.URL.Path != "/api/tags" {
			if _, ok := models[model]; !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":"model %q not found"}`, model)
				return
			}
			time.Sleep(delay)
			f.mu.Lock()
			f.served++
			f.mu.Unlock()
		}
		switch r.URL.Path {
		case "/api/tags":
			var list []map[string]any
			for name, caps := range models {
				list = append(list, map[string]any{"name": name, "size": 800_000_000, "capabilities": caps})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/generate":
			// "Summary of <file>: <its first word>", streamed in two pieces.
			prompt, _ := req["prompt"].(string)
			name := "?"
			if m := docHeader.FindStringSubmatch(prompt); m != nil {
				name = m[1]
			}
			body := strings.SplitN(prompt, "\n", 3)
			first := strings.Fields(body[1])[0]
			for _, piece := range []string{"Summary of " + name, ": " + first} {
				b, _ := json.Marshal(map[string]any{"response": piece})
				w.Write(append(b, '\n'))
				w.(http.Flusher).Flush()
			}
			w.Write([]byte(`{"done":true,"eval_count":5,"eval_duration":1000000000,"done_reason":"stop"}` + "\n"))
		case "/api/chat":
			// A classification: the first label the document mentions; a
			// document saying MISBEHAVE gets an answer outside the labels.
			format, _ := req["format"].(map[string]any)
			props, _ := format["properties"].(map[string]any)
			label, _ := props["label"].(map[string]any)
			enum, _ := label["enum"].([]any)
			msgs, _ := req["messages"].([]any)
			user, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string)
			doc := strings.ToLower(user[strings.Index(user, "<<<"):])
			answer := fmt.Sprint(enum[0])
			for _, l := range enum {
				if strings.Contains(doc, strings.ToLower(fmt.Sprint(l))) {
					answer = fmt.Sprint(l)
					break
				}
			}
			if strings.Contains(doc, "misbehave") {
				answer = "something else"
			}
			content, _ := json.Marshal(map[string]string{"label": answer})
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}, "done": true, "done_reason": "stop"})
		case "/api/embed":
			input, _ := req["input"].(string)
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{float32(len(input)), 0.5}}, "prompt_eval_count": 3})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// heartbeating waits until every agent is READY and has heartbeated
// (placement needs live memory for the AI types' reservation).
func heartbeating(t *testing.T, m artifactManager, agents ...*agent.Agent) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		for _, a := range agents {
			rec, ok := m.srv.Registry.Get(a.NodeID())
			if !ok || rec.State != domain.NodeReady || rec.LastMetrics.LastHeartbeat.IsZero() {
				return false
			}
		}
		return true
	})
}

// perFileJob is a job running capability once per file (named after it),
// combined by report.collect into report.
func perFileJob(t *testing.T, api, capability string, params map[string]string, report string, files map[string]string, order ...string) map[string]any {
	t.Helper()
	var tasks []map[string]any
	for _, name := range order {
		sha := uploadArtifact(t, api, []byte(files[name]))
		tasks = append(tasks, map[string]any{"name": name, "capability": capability, "params": params,
			"inputs": []map[string]string{{"name": name, "sha256": sha}}})
	}
	return map[string]any{"tasks": tasks, "reduce": map[string]any{"capability": "report.collect", "params": map[string]string{"name": report}}}
}

// The headline case: summarise a folder over two devices, one report with
// each summary under the file it came from (the parts themselves are only
// numbered: parts/0003/response.txt).
func TestBatchSummariesSpreadAndAreCollectedByFileName(t *testing.T) {
	const addr = "127.0.0.1:19630"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fa := &batchAIOllama{models: map[string][]string{"gemma3:1b": {"completion"}}, delay: 400 * time.Millisecond}
	fb := &batchAIOllama{models: map[string][]string{"gemma3:1b": {"completion"}}, delay: 400 * time.Millisecond}
	a := startLLMAgent(t, ctx, addr, "summary-a", startBatchAIOllama(t, fa), 2)
	b := startLLMAgent(t, ctx, addr, "summary-b", startBatchAIOllama(t, fb), 2)
	heartbeating(t, m, a, b)

	files := map[string]string{"alpha.txt": "apples grow on trees", "beta_notes.md": "bees make honey", "gamma.txt": "goats climb", "delta.txt": "ducks swim"}
	order := []string{"alpha.txt", "beta_notes.md", "gamma.txt", "delta.txt"}
	body := perFileJob(t, m.api, "llm.generate", map[string]string{"model": "gemma3:1b", "prompt": "Summarise the document above."}, "summaries.md", files, order...)
	// A task without a name (an API client's): named after its file.
	body["tasks"].([]map[string]any)[3]["name"] = ""
	code, job, raw := postJob(t, m.api, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	j := waitJob(t, m.api, job.ID, 60*time.Second)
	if j.State != domain.JobCompleted || len(j.Outputs) != 1 || j.Outputs[0].Name != "summaries.md" {
		t.Fatalf("job %+v", j)
	}
	report := downloadText(t, m.api, j.Outputs[0].SHA256)
	want := "## alpha.txt\n\nSummary of alpha.txt: apples\n\n## beta_notes.md\n\nSummary of beta_notes.md: bees\n\n" +
		"## gamma.txt\n\nSummary of gamma.txt: goats\n\n## delta.txt\n\nSummary of delta.txt: ducks\n\n"
	if report != want {
		t.Fatalf("report:\n%s\nwant:\n%s", report, want)
	}
	nodes := map[domain.NodeID]bool{}
	for _, task := range getJob(t, m.api, job.ID).Tasks {
		nodes[task.Node] = true
	}
	if len(nodes) != 2 || fa.served == 0 || fb.served == 0 {
		t.Fatalf("summaries ran on %v (a served %d, b %d), want both devices", nodes, fa.served, fb.served)
	}

	// The index is the manager's: a reduce input of the client's under its
	// name is refused up front, as is a report over results it can't read.
	bad := perFileJob(t, m.api, "llm.generate", map[string]string{"model": "gemma3:1b", "prompt": "x"}, "r.md", files, "alpha.txt")
	bad["reduce"].(map[string]any)["inputs"] = []map[string]string{{"name": "parts/tasks.json", "sha256": uploadArtifact(t, m.api, []byte(`{"tasks":[]}`))}}
	if code, _, raw := postJob(t, m.api, bad); code != http.StatusBadRequest || !strings.Contains(raw, "parts/tasks.json") {
		t.Fatalf("a client's parts/tasks.json: %d %s", code, raw)
	}
	photos := perFileJob(t, m.api, "image.resize", map[string]string{"width": "10"}, "r.md", map[string]string{"p.png": string(pngBytes(20, 20))}, "p.png")
	if code, _, raw := postJob(t, m.api, photos); code != http.StatusBadRequest || !strings.Contains(raw, "report.collect accepts") {
		t.Fatalf("a report over images: %d %s", code, raw)
	}
}

func TestBatchClassifyCollectsLabelsAndChecksTheAnswer(t *testing.T) {
	const addr = "127.0.0.1:19631"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := &batchAIOllama{models: map[string][]string{"gemma3:1b": {"completion"}}}
	a := startLLMAgent(t, ctx, addr, "classifier", startBatchAIOllama(t, f), 2)
	heartbeating(t, m, a)

	files := map[string]string{"bill.txt": "Invoice no. 7, please pay", "shop.txt": "Receipt: paid in cash", "letter.txt": "Dear sir, a letter"}
	params := map[string]string{"model": "gemma3:1b", "labels": "invoice, receipt, letter"}
	code, job, raw := postJob(t, m.api, perFileJob(t, m.api, "llm.classify", params, "labels.csv", files, "bill.txt", "shop.txt", "letter.txt"))
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	j := waitJob(t, m.api, job.ID, 60*time.Second)
	if j.State != domain.JobCompleted || len(j.Outputs) != 1 {
		t.Fatalf("job %+v", j)
	}
	if got := downloadText(t, m.api, j.Outputs[0].SHA256); got != "file,label\nbill.txt,invoice\nshop.txt,receipt\nletter.txt,letter\n" {
		t.Fatalf("labels.csv:\n%s", got)
	}
	// Labels are checked at submit, not after the work ran.
	for labels, want := range map[string]string{"spam": "give 2-32 labels", "spam,ham,Spam": "given twice", "spam,=1+1": "starting with a letter or digit"} {
		code, out := postWorkload(t, m.api, map[string]any{"capability": "llm.classify", "params": map[string]string{"model": "gemma3:1b", "labels": labels},
			"inputs": []map[string]string{{"name": "a.txt", "sha256": uploadArtifact(t, m.api, []byte("text"))}}})
		if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["raw"]), want) {
			t.Errorf("labels %q: %d %v", labels, code, out)
		}
	}
	// An answer outside the labels fails its task (the format should rule
	// it out; the agent checks anyway), and with it the job.
	bad := perFileJob(t, m.api, "llm.classify", params, "labels.csv", map[string]string{"odd.txt": "MISBEHAVE"}, "odd.txt")
	bad["maxAttempts"] = 1
	code, job, raw = postJob(t, m.api, bad)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	j = waitJob(t, m.api, job.ID, 30*time.Second)
	if j.State != domain.JobFailed || len(j.Outputs) != 0 {
		t.Fatalf("a model answering outside the labels: %+v", j)
	}
	if task := getJob(t, m.api, job.ID).Tasks[0]; !strings.Contains(task.Error, `answered "something else", which isn't one of the labels`) {
		t.Fatalf("task error %q", task.Error)
	}
}

// Embeddings go only to a device with an embedding model, and a prompt
// never to an embedding model: each type's own model list is what
// placement matches.
func TestBatchEmbedOnlyWhereAnEmbeddingModelIs(t *testing.T) {
	const addr = "127.0.0.1:19632"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	text := &batchAIOllama{models: map[string][]string{"gemma3:1b": {"completion"}}}
	// The vector box has both kinds, as a real device often does.
	vec := &batchAIOllama{models: map[string][]string{"embeddinggemma:latest": {"embedding"}, "gemma3:1b": {"completion"}}}
	a := startLLMAgent(t, ctx, addr, "text-box", startBatchAIOllama(t, text), 2)
	b := startLLMAgent(t, ctx, addr, "vector-box", startBatchAIOllama(t, vec), 2)
	heartbeating(t, m, a, b)

	var cat struct {
		Types []struct {
			Name    string              `json:"name"`
			Nodes   int                 `json:"nodes"`
			Choices map[string][]string `json:"choices"`
		} `json:"types"`
	}
	getJSON(t, m.api+"/catalog", &cat)
	seen := 0
	for _, ty := range cat.Types {
		switch ty.Name {
		case "llm.embed":
			seen++
			if ty.Nodes != 1 || strings.Join(ty.Choices["model"], ",") != "embeddinggemma:latest" {
				t.Fatalf("llm.embed in the catalog: %+v", ty)
			}
		case "llm.generate", "llm.classify":
			seen++
			if ty.Nodes != 2 || strings.Join(ty.Choices["model"], ",") != "gemma3:1b" {
				t.Fatalf("%s in the catalog: %+v", ty.Name, ty)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("catalog %+v", cat)
	}
	sha := uploadArtifact(t, m.api, []byte("some text"))
	in := []map[string]string{{"name": "a.txt", "sha256": sha}}
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "llm.embed", "params": map[string]string{"model": "gemma3:1b"}, "inputs": in}); code != http.StatusConflict {
		t.Fatalf("embeddings with a text model: %d %v", code, out)
	}
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "llm.generate", "params": map[string]string{"model": "embeddinggemma", "prompt": "x"}}); code != http.StatusConflict {
		t.Fatalf("a prompt to an embedding model: %d %v", code, out)
	}

	files := map[string]string{"a.txt": "one", "b.txt": "three", "c.txt": "seventeen"}
	code, job, raw := postJob(t, m.api, perFileJob(t, m.api, "llm.embed", map[string]string{"model": "embeddinggemma"}, "embeddings.json", files, "a.txt", "b.txt", "c.txt"))
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	j := waitJob(t, m.api, job.ID, 60*time.Second)
	if j.State != domain.JobCompleted || len(j.Outputs) != 1 {
		t.Fatalf("job %+v", j)
	}
	for _, task := range getJob(t, m.api, job.ID).Tasks {
		if task.Node != b.NodeID() {
			t.Fatalf("an embedding ran on %s, not the device with the model", task.Node)
		}
	}
	var got []struct {
		Name   string    `json:"name"`
		Model  string    `json:"model"`
		Vector []float64 `json:"vector"`
	}
	report := downloadText(t, m.api, j.Outputs[0].SHA256)
	if err := json.Unmarshal([]byte(report), &got); err != nil || len(got) != 3 || got[0].Name != "a.txt" || got[2].Name != "c.txt" ||
		got[1].Vector[0] != 5 || got[2].Model != "embeddinggemma:latest" {
		t.Fatalf("embeddings.json %v:\n%s", err, report)
	}
	if text.served != 0 {
		t.Fatalf("the text-only device was asked %d time(s)", text.served)
	}
}
