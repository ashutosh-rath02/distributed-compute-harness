package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

type workflowStageJSON struct {
	Stage   int                  `json:"stage"`
	Type    string               `json:"type"`
	State   string               `json:"state"`
	Job     string               `json:"job"`
	Error   string               `json:"error"`
	Outputs []domain.ArtifactRef `json:"outputs"`
	Counts  *struct {
		Total, Completed, Active int
	} `json:"counts"`
}

type workflowJSON struct {
	ID      string               `json:"id"`
	State   domain.JobState      `json:"state"`
	Error   string               `json:"error"`
	Stage   int                  `json:"stage"`
	Stages  []workflowStageJSON  `json:"stages"`
	Outputs []domain.ArtifactRef `json:"outputs"`
}

// callAPI sends body (JSON, if any) with an optional bearer.
func callAPI(t *testing.T, bearer, method, url string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func postWorkflow(t *testing.T, bearer, api string, body map[string]any) (int, workflowJSON, string) {
	t.Helper()
	code, raw := callAPI(t, bearer, http.MethodPost, api+"/workflows", body)
	var wf workflowJSON
	json.Unmarshal(raw, &wf)
	return code, wf, string(raw)
}

func getWorkflow(t *testing.T, bearer, api, id string) workflowJSON {
	t.Helper()
	_, raw := callAPI(t, bearer, http.MethodGet, api+"/workflows/"+id, nil)
	var wf workflowJSON
	json.Unmarshal(raw, &wf)
	return wf
}

func waitWorkflow(t *testing.T, bearer, api, id string, timeout time.Duration) workflowJSON {
	t.Helper()
	var wf workflowJSON
	waitFor(t, timeout, func() bool { wf = getWorkflow(t, bearer, api, id); return wf.State != domain.JobRunning })
	return wf
}

func uploadPhotos(t *testing.T, bearer, api string, names ...string) []map[string]string {
	t.Helper()
	var files []map[string]string
	for i, name := range names {
		req, _ := http.NewRequest(http.MethodPost, api+"/artifacts", bytes.NewReader(pngBytes(120+i, 80)))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var info struct{ SHA256 string }
		json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %s", name, resp.Status)
		}
		files = append(files, map[string]string{"name": name, "sha256": info.SHA256})
	}
	return files
}

func zipNames(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func waitTwoFileAgents(t *testing.T, ctx context.Context, m artifactManager, addr, prefix string) {
	t.Helper()
	a := startFileAgentWithSlots(t, ctx, m, addr, prefix+"-a", 2)
	b := startFileAgentWithSlots(t, ctx, m, addr, prefix+"-b", 2)
	waitFor(t, 5*time.Second, func() bool {
		ra, okA := m.srv.Registry.Get(a.NodeID())
		rb, okB := m.srv.Registry.Get(b.NodeID())
		return okA && okB && ra.State == domain.NodeReady && rb.State == domain.NodeReady &&
			!ra.LastMetrics.LastHeartbeat.IsZero() && !rb.LastMetrics.LastHeartbeat.IsZero()
	})
}

// The headline: photos resized on the fleet, zipped, the zip checksummed —
// three stages, each an ordinary job on the results of the one before.
func TestWorkflowResizesZipsAndHashes(t *testing.T) {
	const addr = "127.0.0.1:19620"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	waitTwoFileAgents(t, ctx, m, addr, "wf-photos")

	files := uploadPhotos(t, "", m.api, "beach.png", "city.png", "dog.png")
	code, wf, raw := postWorkflow(t, "", m.api, map[string]any{"name": "photos", "inputs": files, "stages": []map[string]any{
		{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "60"}},
		{"type": "archive.zip", "params": map[string]string{"name": "small.zip"}},
		{"type": "file.hash"},
	}})
	if code != http.StatusAccepted || wf.State != domain.JobRunning || len(wf.Stages) != 3 {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	wf = waitWorkflow(t, "", m.api, wf.ID, 90*time.Second)
	if wf.State != domain.JobCompleted || wf.Stage != 3 {
		t.Fatalf("workflow %s (%s) %+v", wf.State, wf.Error, wf.Stages)
	}
	for _, st := range wf.Stages {
		if st.State != "COMPLETED" || st.Job == "" {
			t.Fatalf("stage %+v", st)
		}
	}
	if c := wf.Stages[0].Counts; c == nil || c.Total != 3 || c.Completed != 3 {
		t.Fatalf("stage 1 counts %+v", c)
	}
	zipRef := wf.Stages[1].Outputs
	if len(zipRef) != 1 || zipRef[0].Name != "small.zip" {
		t.Fatalf("stage 2 handed on %+v", zipRef)
	}
	_, zipBody := callAPI(t, "", http.MethodGet, m.api+"/artifacts/"+zipRef[0].SHA256, nil)
	if got := zipNames(t, zipBody); got != "beach-60.jpg,city-60.jpg,dog-60.jpg" {
		t.Fatalf("zip holds %s", got)
	}
	if len(wf.Outputs) != 1 || wf.Outputs[0].Name != "hashes.txt" {
		t.Fatalf("results %+v", wf.Outputs)
	}
	if got := downloadText(t, m.api, wf.Outputs[0].SHA256); got != zipRef[0].SHA256+"  small.zip\n" {
		t.Fatalf("hashes.txt %q, want the zip's checksum %s", got, zipRef[0].SHA256)
	}
	// Each stage is an ordinary job, listed with the rest.
	var jobs []struct{ Name string }
	getJSON(t, m.api+"/jobs", &jobs)
	if len(jobs) != 3 || !strings.Contains(jobs[1].Name, "workflow step 2 of 3") {
		t.Fatalf("jobs %+v", jobs)
	}
}

func decodePNG(t *testing.T, body []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Strips rendered across devices, stacked, resized: the stack must get
// the strips in order (fractal-10.png sorts before fractal-2.png by name),
// so the stacked picture is the one rendered in a single piece.
func TestWorkflowStacksStripsInOrder(t *testing.T) {
	const addr = "127.0.0.1:19621"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	waitTwoFileAgents(t, ctx, m, addr, "wf-strips")

	params := map[string]string{"width": "96", "height": "120", "iterations": "300", "scene": "classic"}
	code, wf, raw := postWorkflow(t, "", m.api, map[string]any{"stages": []map[string]any{
		{"type": "render.fractal", "mode": "parts", "parts": 12, "params": params},
		{"type": "image.stack", "params": map[string]string{"name": "whole.png"}},
		{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "48", "format": "png"}},
	}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	_, one, raw := postWorkflow(t, "", m.api, map[string]any{"stages": []map[string]any{
		{"type": "render.fractal", "mode": "parts", "parts": 1, "params": params},
	}})
	if one.ID == "" {
		t.Fatalf("single render: %s", raw)
	}
	wf = waitWorkflow(t, "", m.api, wf.ID, 90*time.Second)
	one = waitWorkflow(t, "", m.api, one.ID, 90*time.Second)
	if wf.State != domain.JobCompleted || one.State != domain.JobCompleted {
		t.Fatalf("workflows %s (%s), %s (%s)", wf.State, wf.Error, one.State, one.Error)
	}
	if len(wf.Outputs) != 1 || wf.Outputs[0].Name != "whole-48.png" || len(one.Outputs) != 1 || one.Outputs[0].Name != "fractal-0.png" {
		t.Fatalf("results %+v and %+v", wf.Outputs, one.Outputs)
	}
	_, stackedBody := callAPI(t, "", http.MethodGet, m.api+"/artifacts/"+wf.Stages[1].Outputs[0].SHA256, nil)
	_, wholeBody := callAPI(t, "", http.MethodGet, m.api+"/artifacts/"+one.Outputs[0].SHA256, nil)
	stacked, whole := decodePNG(t, stackedBody), decodePNG(t, wholeBody)
	if stacked.Bounds() != whole.Bounds() {
		t.Fatalf("stacked %v, whole %v", stacked.Bounds(), whole.Bounds())
	}
	for y := 0; y < whole.Bounds().Dy(); y++ {
		for x := 0; x < whole.Bounds().Dx(); x++ {
			r1, g1, b1, _ := stacked.At(x, y).RGBA()
			r2, g2, b2, _ := whole.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 {
				t.Fatalf("the stacked strips differ from the whole picture at %d,%d: strips out of order", x, y)
			}
		}
	}
}

// Whatever can't run is refused before anything runs; a type disabled
// while an earlier stage runs fails the workflow with the reason when its
// stage comes.
func TestWorkflowRefusedUpFrontOrWhenPolicyChanges(t *testing.T) {
	const addr = "127.0.0.1:19622"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "wf-policy", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	files := uploadPhotos(t, "", m.api, "a.png", "b.png", "c.png")
	resize := map[string]any{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "40"}}
	nine := make([]map[string]any, 9)
	for i := range nine {
		nine[i] = map[string]any{"type": "file.hash"}
	}
	for name, tc := range map[string]struct {
		stages []map[string]any
		code   int
		want   string
	}{
		"raw command": {[]map[string]any{resize, {"type": "system.execute", "params": map[string]string{"command": "x"}}}, http.StatusBadRequest, "isn't a built-in task type"},
		"nine steps":  {nine, http.StatusBadRequest, "1-8 steps"},
		"one file per run, given all at once": {[]map[string]any{resize, {"type": "image.resize", "mode": "once", "params": map[string]string{"width": "20"}}},
			http.StatusBadRequest, "image.resize takes 1-1 input files, got 3"},
	} {
		code, _, raw := postWorkflow(t, "", m.api, map[string]any{"inputs": files, "stages": tc.stages})
		if code != tc.code || !strings.Contains(raw, tc.want) {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	// A misspelled field isn't dropped without a word.
	if code, _, raw := postWorkflow(t, "", m.api, map[string]any{"inputs": files, "stages": []map[string]any{{"type": "file.hash", "parms": map[string]string{}}}}); code != http.StatusBadRequest || !strings.Contains(raw, "parms") {
		t.Errorf("unknown field: %d %s", code, raw)
	}
	p := domain.PermissivePolicy()
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"file.hash": {Enabled: false}}
	putPolicy(t, m.api, p)
	if code, _, raw := postWorkflow(t, "", m.api, map[string]any{"inputs": files, "stages": []map[string]any{resize, {"type": "archive.zip"}, {"type": "file.hash"}}}); code != http.StatusForbidden || !strings.Contains(raw, "step 3 (Checksum files)") {
		t.Errorf("disabled at step 3: %d %s", code, raw)
	}
	var jobs, wfs []any
	getJSON(t, m.api+"/jobs", &jobs)
	getJSON(t, m.api+"/workflows", &wfs)
	if len(jobs) != 0 || len(wfs) != 0 {
		t.Fatalf("refused workflows left %d jobs, %d workflows", len(jobs), len(wfs))
	}
	putPolicy(t, m.api, domain.PermissivePolicy())

	// Hold the only slot, so stage 1 waits while the policy changes.
	code, out := postWorkload(t, m.api, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "60", "threads": "1"}})
	if code != http.StatusAccepted {
		t.Fatalf("hold: %d %v", code, out)
	}
	hold := out["id"].(string)
	code, wf, raw := postWorkflow(t, "", m.api, map[string]any{"inputs": files[:2], "stages": []map[string]any{resize, {"type": "archive.zip"}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	waitFor(t, 10*time.Second, func() bool { return getWorkflow(t, "", m.api, wf.ID).Stages[0].Job != "" })
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"archive.zip": {Enabled: false}}
	putPolicy(t, m.api, p)
	resp, err := http.Post(m.api+"/workloads/"+hold+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wf = waitWorkflow(t, "", m.api, wf.ID, 45*time.Second)
	if wf.State != domain.JobFailed || !strings.Contains(wf.Error, "step 2 (Zip files together)") || !strings.Contains(wf.Error, "archive.zip is disabled") {
		t.Fatalf("workflow %s: %q", wf.State, wf.Error)
	}
	if wf.Stages[0].State != "COMPLETED" || wf.Stages[1].State != "SKIPPED" || wf.Stages[1].Job != "" {
		t.Fatalf("stages %+v", wf.Stages)
	}
}

// Cancel stops the running step's task on its device.
func TestCancelWorkflowStopsItsRunningStep(t *testing.T) {
	const addr = "127.0.0.1:19623"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "wf-cancel")
	code, wf, raw := postWorkflow(t, "", m.api, map[string]any{"stages": []map[string]any{{"type": "cpu.burn", "params": map[string]string{"seconds": "60", "threads": "1"}}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	// Running on the device, not just assigned.
	var task struct {
		Tasks []struct{ Workload string }
	}
	var running map[string]any
	waitFor(t, 10*time.Second, func() bool {
		st := getWorkflow(t, "", m.api, wf.ID).Stages[0]
		if st.Job == "" {
			return false
		}
		getJSON(t, m.api+"/jobs/"+st.Job, &task)
		if len(task.Tasks) != 1 || task.Tasks[0].Workload == "" {
			return false
		}
		getJSON(t, m.api+"/workloads/"+task.Tasks[0].Workload, &running)
		return running["state"] == "RUNNING"
	})
	start := time.Now()
	if code, raw := callAPI(t, "", http.MethodPost, m.api+"/workflows/"+wf.ID+"/cancel", nil); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", code, raw)
	}
	wf = getWorkflow(t, "", m.api, wf.ID)
	if wf.State != domain.JobCanceled || wf.Stages[0].State != "CANCELED" {
		t.Fatalf("after cancel: %s %+v", wf.State, wf.Stages)
	}
	if v := waitState(t, m.api, task.Tasks[0].Workload, 15*time.Second); v["state"] != "CANCELED" {
		t.Fatalf("the step's task: %v", v["state"])
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the task took %v to stop", time.Since(start))
	}
	if code, _ := callAPI(t, "", http.MethodPost, m.api+"/workflows/nope/cancel", nil); code != http.StatusNotFound {
		t.Fatalf("unknown workflow: %d", code)
	}
}

// A manager restart mid-workflow loses nothing and repeats nothing: the
// workflow goes on from its stage, one job per stage.
func TestWorkflowResumesAfterManagerRestart(t *testing.T) {
	const addr = "127.0.0.1:19624"
	dbPath := filepath.Join(t.TempDir(), "workflows.db")
	store := openArtifactStore(t, 1<<20)
	start := func() (*manager.Server, string, func()) {
		db, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		transport := ws.New()
		srv := manager.NewServer(transport, db, manager.Config{
			Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, ReconcileInterval: 100 * time.Millisecond, Artifacts: store,
		})
		transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
		go srv.Run(ctx)
		waitListening(t, addr)
		api := httptest.NewServer(srv.NewHTTPHandler())
		return srv, api.URL, func() { api.Close(); cancel(); time.Sleep(150 * time.Millisecond); db.Close() }
	}
	srv1, api1, stop1 := start()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, artifactManager{srv: srv1, api: api1}, addr, "wf-restart", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv1.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	// The only slot held: stage 1's tasks wait in the queue over the restart.
	if code, out := postWorkload(t, api1, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "60", "threads": "1"}}); code != http.StatusAccepted {
		t.Fatalf("hold: %d %v", code, out)
	}
	files := uploadPhotos(t, "", api1, "a.png", "b.png")
	code, wf, raw := postWorkflow(t, "", api1, map[string]any{"inputs": files, "stages": []map[string]any{
		{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "40"}},
		{"type": "archive.zip", "params": map[string]string{"name": "small.zip"}},
	}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	waitFor(t, 10*time.Second, func() bool {
		st := getWorkflow(t, "", api1, wf.ID).Stages[0]
		return st.Counts != nil && st.Counts.Total == 2 && st.Counts.Active == 2
	})
	stop1()

	_, api2, stop2 := start()
	defer stop2()
	wf = waitWorkflow(t, "", api2, wf.ID, 60*time.Second)
	if wf.State != domain.JobCompleted || len(wf.Outputs) != 1 || wf.Outputs[0].Name != "small.zip" {
		t.Fatalf("after a restart: %s (%s) %+v", wf.State, wf.Error, wf.Outputs)
	}
	var jobs []struct{ Name string }
	getJSON(t, api2+"/jobs", &jobs)
	if len(jobs) != 2 {
		t.Fatalf("one job per stage expected, got %+v", jobs)
	}
	_, zipBody := callAPI(t, "", http.MethodGet, api2+"/artifacts/"+wf.Outputs[0].SHA256, nil)
	if got := zipNames(t, zipBody); got != "a-40.jpg,b-40.jpg" {
		t.Fatalf("zip holds %s", got)
	}
}

// A plan with follow-up steps becomes a workflow on approval; one without
// stays the one job it always was. Workflows need operator credentials.
func TestPlanWithFollowUpStepsRunsAsAWorkflow(t *testing.T) {
	const addr = "127.0.0.1:19625"
	f := &plannerOllama{answer: func(request string) string {
		switch {
		case strings.Contains(request, "checksum"):
			return `{"summary":"Resize, zip, checksum.","task":{"type":"image.resize","params":{"width":60}},"files":["beach.png","city.png"],"mode":"perFile","parts":0,"combine":null,` +
				`"then":[{"type":"archive.zip","params":{"name":"small.zip"},"mode":"once"},{"type":"file.hash","params":{},"mode":"once"}],"possible":true,"reason":""}`
		case strings.Contains(request, "strips"):
			return `{"summary":"x","task":{"type":"image.resize","params":{"width":60}},"files":[],"mode":"perFile","parts":0,"combine":null,` +
				`"then":[{"type":"render.fractal","params":{},"mode":"once"}],"possible":true,"reason":""}`
		case strings.Contains(request, "stack"):
			// Each step is a type the planner may use, but a zip is no
			// image: only checking the whole workflow finds that.
			return `{"summary":"x","task":{"type":"image.resize","params":{"width":60}},"files":[],"mode":"perFile","parts":0,"combine":null,` +
				`"then":[{"type":"archive.zip","params":{},"mode":"once"},{"type":"image.stack","params":{},"mode":"once"}],"possible":true,"reason":""}`
		}
		return `{"summary":"Zip them.","task":{"type":"archive.zip","params":{"name":"all.zip"}},"files":[],"mode":"once","parts":0,"combine":null,"then":[],"possible":true,"reason":""}`
	}}
	m, _ := startPlannerFleet(t, addr, f)
	files := uploadPhotos(t, testOperatorToken, m.api, "beach.png", "city.png")

	// Workflows are operator-only: no credentials, or the AI key, get nothing.
	for _, bearer := range []string{"", testAIKey} {
		if code, _ := callAPI(t, bearer, http.MethodGet, m.api+"/workflows", nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /workflows with %q: %d", bearer, code)
		}
		if code, _, _ := postWorkflow(t, bearer, m.api, map[string]any{"stages": []map[string]any{{"type": "cpu.burn"}}}); code != http.StatusUnauthorized {
			t.Fatalf("POST /workflows with %q: %d", bearer, code)
		}
	}

	_, p, raw := postPlan(t, m.api, "resize, zip them, then checksum the zip", files)
	if p.ID == "" {
		t.Fatalf("POST /plans: %s", raw)
	}
	var plan struct {
		State, Problem, WorkflowID, JobID string
		Then                              []struct {
			Type, Mode string
			Runs       int
			Effective  map[string]string
		}
	}
	waitFor(t, 20*time.Second, func() bool {
		_, raw := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/plans/"+p.ID, "", nil)
		json.Unmarshal(raw, &plan)
		return plan.State != "planning"
	})
	if plan.State != "proposed" || len(plan.Then) != 2 || plan.Then[0].Type != "archive.zip" || plan.Then[0].Runs != 1 || plan.Then[0].Effective["name"] != "small.zip" || plan.Then[1].Effective["algorithm"] != "sha256" {
		t.Fatalf("plan %+v", plan)
	}
	if code, raw := callAPI(t, testOperatorToken, http.MethodGet, m.api+"/workflows", nil); code != http.StatusOK || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("a workflow exists before approval: %s", raw)
	}
	code, raw2 := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil)
	json.Unmarshal(raw2, &plan)
	if code != http.StatusAccepted || plan.State != "approved" || plan.WorkflowID == "" || plan.JobID != "" {
		t.Fatalf("approve: %d %s", code, raw2)
	}
	wf := waitWorkflow(t, testOperatorToken, m.api, plan.WorkflowID, 60*time.Second)
	if wf.State != domain.JobCompleted || len(wf.Outputs) != 1 || wf.Outputs[0].Name != "hashes.txt" || wf.Stages[1].Outputs[0].Name != "small.zip" {
		t.Fatalf("planned workflow %s (%s) %+v", wf.State, wf.Error, wf.Outputs)
	}

	// A follow-up that can't take the files before it: refused, not approvable.
	_, p, _ = postPlan(t, m.api, "render strips after", files)
	refused := waitPlan(t, m.api, p.ID, settled)
	if refused.State != "refused" || !strings.Contains(refused.Problem, "render.fractal takes no files") {
		t.Fatalf("refused plan %+v", refused)
	}
	_, p, _ = postPlan(t, m.api, "zip, then stack the zip", files)
	refused = waitPlan(t, m.api, p.ID, settled)
	if refused.State != "refused" || !strings.Contains(refused.Problem, "step 3 (Stack image strips)") || !strings.Contains(refused.Problem, "not archive.zip") {
		t.Fatalf("plan with a step that can't take the files before it %+v", refused)
	}
	if code, _ := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil); code != http.StatusConflict {
		t.Fatalf("approving a refused plan: %d", code)
	}

	// No follow-ups: a job, as before.
	_, p, _ = postPlan(t, m.api, "zip them", files)
	if one := waitPlan(t, m.api, p.ID, settled); one.State != "proposed" {
		t.Fatalf("one-step plan %+v", one)
	}
	_, raw3 := asBearer(t, testOperatorToken, http.MethodPost, m.api+"/plans/"+p.ID+"/approve", "", nil)
	plan.WorkflowID, plan.JobID = "", ""
	json.Unmarshal(raw3, &plan)
	if plan.JobID == "" || plan.WorkflowID != "" {
		t.Fatalf("one-step plan approved as %s", raw3)
	}

	_, audit := asBearer(t, testOperatorToken, http.MethodGet, m.api+"/audit?limit=100", "", nil)
	for _, kind := range []string{"workflow.submitted", "plan.approved"} {
		if !strings.Contains(string(audit), kind) {
			t.Errorf("audit lacks %s", kind)
		}
	}
}
