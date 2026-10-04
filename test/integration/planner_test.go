package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// plannerOllama is a local model that plans: /api/chat answers with
// whatever answer() returns for the request (the last user message),
// optionally slowly, and records what each chat asked for.
type plannerOllama struct {
	mu     sync.Mutex
	answer func(request string) string
	delay  time.Duration
	chats  []plannerChat
	cut    int // answers cut off by the caller going away
}

type plannerChat struct {
	System, User string
	Format       json.RawMessage
}

func (f *plannerOllama) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"gemma3:1b","size":800000000}]}`))
		case "/api/chat":
			var req struct {
				Messages []struct{ Role, Content string }
				Format   json.RawMessage
			}
			json.NewDecoder(r.Body).Decode(&req)
			var c plannerChat
			for _, m := range req.Messages {
				if m.Role == "system" {
					c.System = m.Content
				} else {
					c.User = m.Content
				}
			}
			c.Format = req.Format
			f.mu.Lock()
			f.chats = append(f.chats, c)
			answer, delay := f.answer(c.User), f.delay
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
				f.mu.Lock()
				f.cut++
				f.mu.Unlock()
				return
			case <-time.After(delay):
			}
			b, _ := json.Marshal(map[string]any{"message": map[string]string{"role": "assistant", "content": answer}})
			w.Write(append(b, '\n'))
			w.Write([]byte(`{"done":true,"prompt_eval_count":900,"eval_count":80,"eval_duration":1000000000,"done_reason":"stop"}` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// asOperator calls the manager's API with the operator token (or another
// bearer).
func asBearer(t *testing.T, bearer, method, url, contentType string, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

type planJSON struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Summary     string `json:"summary"`
	ModelReason string `json:"modelReason"`
	Problem     string `json:"problem"`
	ChatState   string `json:"chatState"`
	WorkloadID  string `json:"workloadId"`
	Mode        string `json:"mode"`
	Tasks       int    `json:"tasks"`
	JobID       string `json:"jobId"`
	UseFiles    []string
	Task        *struct {
		Type      string            `json:"type"`
		Params    map[string]string `json:"params"`
		Effective map[string]string `json:"effective"`
	} `json:"task"`
	Combine *struct {
		Type   string            `json:"type"`
		Params map[string]string `json:"params"`
	} `json:"combine"`
}

func postPlan(t *testing.T, api, request string, files []map[string]string) (int, planJSON, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"request": request, "model": "gemma3:1b", "files": files})
	code, raw := asBearer(t, testOperatorToken, http.MethodPost, api+"/plans", "application/json", body)
	var p planJSON
	json.Unmarshal(raw, &p)
	return code, p, string(raw)
}

func waitPlan(t *testing.T, api, id string, done func(p planJSON) bool) planJSON {
	t.Helper()
	var p planJSON
	waitFor(t, 20*time.Second, func() bool {
		_, raw := asBearer(t, testOperatorToken, http.MethodGet, api+"/plans/"+id, "", nil)
		json.Unmarshal(raw, &p)
		return done(p)
	})
	return p
}

func settled(p planJSON) bool { return p.State != "planning" }

func startPlannerFleet(t *testing.T, addr string, f *plannerOllama) (artifactManager, context.Context) {
	t.Helper()
	m := startAIManager(t, addr, func() *domain.Policy { p := domain.DefaultPolicy(); return &p }())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	a := startLLMAgent(t, ctx, addr, "planner-agent", f.start(t), 4)
	waitFor(t, 10*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && rec.HasCapability("llm.chat") && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	return m, ctx
}

// The headline: photos and a sentence in, a validated plan out, nothing
// run until the operator approves, then an ordinary job does the work.
func TestPlanFromPlainWordsRunsOnlyOnceApproved(t *testing.T) {
	const addr = "127.0.0.1:19610"
	f := &plannerOllama{answer: func(string) string {
		return `{"summary":"Resize each photo to 60 px and zip them.","task":{"type":"image.resize","params":{"width":60}},` +
			`"files":["beach.png","city.png","dog.png"],"mode":"perFile","parts":0,"combine":{"type":"archive.zip","params":{"name":"small.zip"}},"possible":true,"reason":""}`
	}}
	m, _ := startPlannerFleet(t, addr, f)

	var files []map[string]string
	for i, name := range []string{"beach.png", "city.png", "dog.png"} {
		code, raw := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/artifacts", "application/octet-stream", pngBytes(120+i, 80))
		var info struct{ SHA256 string }
		if json.Unmarshal(raw, &info); code != http.StatusCreated {
			t.Fatalf("upload: %d %s", code, raw)
		}
		files = append(files, map[string]string{"name": name, "sha256": info.SHA256})
	}
	const request = "resize these photos to 60 px wide and zip them -- my secret: hunter2"
	code, p, raw := postPlan(t, m.api, request, files)
	if code != http.StatusAccepted || p.State != "planning" || p.WorkloadID == "" {
		t.Fatalf("POST /plans: %d %s", code, raw)
	}
	p = waitPlan(t, m.api, p.ID, settled)
	if p.State != "proposed" || p.Task == nil || p.Task.Type != "image.resize" || p.Mode != "perFile" || p.Tasks != 3 ||
		p.Combine == nil || p.Combine.Type != "archive.zip" || p.Summary != "Resize each photo to 60 px and zip them." {
		t.Fatalf("plan %+v (%s)", p, p.Problem)
	}
	// What will run, defaults filled in by the catalog, not the model.
	if e := p.Task.Effective; e["width"] != "60" || e["format"] != "jpg" || e["quality"] != "85" {
		t.Fatalf("effective params %v", e)
	}

	// The model was offered only what it may use, and the files by name.
	f.mu.Lock()
	chat := f.chats[0]
	f.mu.Unlock()
	if chat.User != request || !strings.Contains(chat.System, "beach.png, city.png, dog.png") || !strings.Contains(chat.System, "image.resize") {
		t.Fatalf("planning chat %+v", chat)
	}
	var schema struct {
		Properties struct {
			Task struct {
				Properties struct {
					Type struct{ Enum []string }
				}
			}
		}
	}
	if err := json.Unmarshal(chat.Format, &schema); err != nil {
		t.Fatalf("no schema reached the model: %v %s", err, chat.Format)
	}
	types := strings.Join(schema.Properties.Task.Properties.Type.Enum, ",")
	for _, never := range []string{"system.execute", "filesystem.read", "llm.chat", "llm.pull", "llm.remove", "llm.split-main", "llm.split-helper", "llm.inventory"} {
		if strings.Contains(","+types+",", ","+never+",") || strings.Contains(chat.System, never+":") {
			t.Errorf("the planner offered %s: %s", never, types)
		}
	}
	if !strings.Contains(types, "image.resize") || !strings.Contains(types, "archive.zip") || !strings.Contains(types, "llm.generate") {
		t.Fatalf("offered types %s", types)
	}

	// Proposed is not run.
	var jobs []any
	_, raw2 := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/jobs", "", nil)
	if json.Unmarshal(raw2, &jobs); len(jobs) != 0 {
		t.Fatalf("a job exists before approval: %s", raw2)
	}
	// The AI key can neither plan nor approve; no credentials at all can't either.
	for _, bearer := range []string{testAIKey, ""} {
		if code, _ := asBearer(t, bearer, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("approve with %q: %d", bearer, code)
		}
		if code, _ := asBearer(t, bearer, http.MethodPost, m.api+"/plans", "application/json", []byte(`{"request":"x","model":"gemma3:1b"}`)); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("plan with %q: %d", bearer, code)
		}
	}

	code, raw3 := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil)
	var approved planJSON
	if json.Unmarshal(raw3, &approved); code != http.StatusAccepted || approved.State != "approved" || approved.JobID == "" {
		t.Fatalf("approve: %d %s", code, raw3)
	}
	if code, _ := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil); code != http.StatusConflict {
		t.Fatalf("approving twice: %d", code)
	}
	var job jobJSON
	waitFor(t, 60*time.Second, func() bool {
		_, raw := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/jobs/"+approved.JobID, "", nil)
		json.Unmarshal(raw, &job)
		return job.State != domain.JobRunning
	})
	if job.State != domain.JobCompleted || len(job.Outputs) != 1 || job.Outputs[0].Name != "small.zip" {
		t.Fatalf("job %s (%s) %+v", job.State, job.Error, job.Outputs)
	}
	_, body := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/artifacts/"+job.Outputs[0].SHA256, "", nil)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "beach-60.jpg,city-60.jpg,dog-60.jpg" {
		t.Fatalf("zip holds %v", names)
	}

	// Audited, without the request's words.
	_, audit := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/audit?limit=100", "", nil)
	for _, kind := range []string{"plan.requested", "plan.approved"} {
		if !strings.Contains(string(audit), kind) {
			t.Errorf("audit lacks %s", kind)
		}
	}
	if strings.Contains(string(audit), "hunter2") {
		t.Fatal("the request text reached the audit log")
	}
}

// Whatever the model proposes beyond what it was offered is refused,
// and a refused plan can't be approved.
func TestPlanThatOverstepsIsRefused(t *testing.T) {
	const addr = "127.0.0.1:19611"
	answers := map[string]string{
		"raw":     `{"summary":"Run a command.","task":{"type":"system.execute","params":{"command":"cmd"}},"files":[],"mode":"once","parts":0,"combine":null,"possible":true,"reason":""}`,
		"range":   `{"summary":"Huge.","task":{"type":"image.resize","params":{"width":99999}},"files":["a.png"],"mode":"perFile","parts":0,"combine":null,"possible":true,"reason":""}`,
		"file":    `{"summary":"Other file.","task":{"type":"image.resize","params":{"width":10}},"files":["C:/Windows/win.ini"],"mode":"perFile","parts":0,"combine":null,"possible":true,"reason":""}`,
		"decline": `{"summary":"","task":{"type":"file.hash","params":{}},"files":[],"mode":"once","parts":0,"combine":null,"possible":false,"reason":"No task type sends email."}`,
		"prose":   `I think you should resize the photos.`,
		"policy":  `{"summary":"Zip.","task":{"type":"archive.zip","params":{}},"files":["a.png"],"mode":"once","parts":0,"combine":null,"possible":true,"reason":""}`,
	}
	f := &plannerOllama{answer: func(request string) string { return answers[request] }}
	m, _ := startPlannerFleet(t, addr, f)
	code, raw := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/artifacts", "application/octet-stream", pngBytes(20, 20))
	var info struct{ SHA256 string }
	if json.Unmarshal(raw, &info); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, raw)
	}
	files := []map[string]string{{"name": "a.png", "sha256": info.SHA256}}

	// Zipping disabled by policy: the planner neither offers it nor
	// accepts it.
	p := domain.DefaultPolicy()
	p.Types["archive.zip"] = domain.TypePolicy{Enabled: false}
	pb, _ := json.Marshal(p)
	if code, raw := asBearer(t, testOperatorToken, http.MethodPut, m.api+"/policy", "application/json", pb); code != http.StatusOK {
		t.Fatalf("PUT /policy: %d %s", code, raw)
	}
	for request, want := range map[string]string{
		"raw":     `"system.execute" isn't a task type the planner may use`,
		"range":   "width",
		"file":    "isn't one of the files given",
		"decline": "can't do this",
		"prose":   "isn't JSON",
		"policy":  `"archive.zip" isn't a task type the planner may use`,
	} {
		code, plan, raw := postPlan(t, m.api, request, files)
		if code != http.StatusAccepted {
			t.Fatalf("%s: POST /plans %d %s", request, code, raw)
		}
		plan = waitPlan(t, m.api, plan.ID, settled)
		if plan.State != "refused" || !strings.Contains(plan.Problem, want) {
			t.Errorf("%s: %s %q, want %q", request, plan.State, plan.Problem, want)
		}
		if code, _ := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+plan.ID+"/approve", "", nil); code != http.StatusConflict {
			t.Errorf("%s: approving a refused plan: %d", request, code)
		}
		if request == "decline" && plan.ModelReason != "No task type sends email." {
			t.Errorf("the model's reason: %q", plan.ModelReason)
		}
	}
	f.mu.Lock()
	last := f.chats[len(f.chats)-1]
	f.mu.Unlock()
	if strings.Contains(string(last.Format), "archive.zip") {
		t.Fatalf("a type disabled by policy was offered: %s", last.Format)
	}
	var jobs []any
	_, raw2 := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/jobs", "", nil)
	if json.Unmarshal(raw2, &jobs); len(jobs) != 0 {
		t.Fatalf("a refused plan ran: %s", raw2)
	}
}

// Planning is bounded: two at once, and rejecting one stops its model.
func TestPlanningIsBoundedAndRejectStopsTheModel(t *testing.T) {
	const addr = "127.0.0.1:19612"
	f := &plannerOllama{delay: time.Minute, answer: func(string) string { return `{}` }}
	m, _ := startPlannerFleet(t, addr, f)
	_, first, raw := postPlan(t, m.api, "one", nil)
	_, second, _ := postPlan(t, m.api, "two", nil)
	if first.ID == "" || second.ID == "" {
		t.Fatalf("plans: %s", raw)
	}
	if code, _, raw := postPlan(t, m.api, "three", nil); code != http.StatusTooManyRequests {
		t.Fatalf("a third plan at once: %d %s", code, raw)
	}
	// The model's device answers one chat at a time: the first is
	// running, the second waits for it.
	first = waitPlan(t, m.api, first.ID, func(p planJSON) bool { return p.ChatState == "RUNNING" })
	code, raw2 := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+first.ID+"/reject", "", nil)
	if code != http.StatusOK || !strings.Contains(string(raw2), `"rejected"`) {
		t.Fatalf("reject: %d %s", code, raw2)
	}
	waitFor(t, 10*time.Second, func() bool {
		var w map[string]any
		_, raw := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/workloads/"+first.WorkloadID, "", nil)
		json.Unmarshal(raw, &w)
		f.mu.Lock()
		defer f.mu.Unlock()
		return w["state"] == "CANCELED" && f.cut == 1
	})
	if p := waitPlan(t, m.api, first.ID, settled); p.State != "rejected" {
		t.Fatalf("after its chat ended the plan is %s", p.State)
	}
	// Its place is free again; the second now runs.
	waitPlan(t, m.api, second.ID, func(p planJSON) bool { return p.ChatState == "RUNNING" })
	if code, third, raw := postPlan(t, m.api, "three", nil); code != http.StatusAccepted {
		t.Fatalf("a plan after one was rejected: %d %s", code, raw)
	} else {
		asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+third.ID+"/reject", "", nil)
	}
	asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+second.ID+"/reject", "", nil)
	if code, _ := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+second.ID+"/reject", "", nil); code != http.StatusConflict {
		t.Fatalf("rejecting twice: %d", code)
	}
}
