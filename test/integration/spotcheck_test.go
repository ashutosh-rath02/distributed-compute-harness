package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// Spot checks (manager/spotcheck.go): a sample of each job's
// deterministic tasks is re-run on another device and the output hashes
// the manager received are compared. The "liar" here is a hostile device
// speaking the real protocol: it fetches its inputs and uploads a wrong
// output with a correct hash header, then reports COMPLETED, so the
// manager has nothing but the spot check to catch it.

type spotTaskJSON struct {
	Key       string               `json:"key"`
	State     string               `json:"state"`
	Attempts  int                  `json:"attempts"`
	Node      domain.NodeID        `json:"node"`
	Outputs   []domain.ArtifactRef `json:"outputs"`
	SpotCheck string               `json:"spotCheck"`
}

type spotJobJSON struct {
	ID         string               `json:"id"`
	State      domain.JobState      `json:"state"`
	Error      string               `json:"error"`
	Tasks      []spotTaskJSON       `json:"tasks"`
	Reduce     *spotTaskJSON        `json:"reduce"`
	Outputs    []domain.ArtifactRef `json:"outputs"`
	SpotChecks *struct {
		Checking, Matched, Mismatch, Unresolved, Skipped int
	} `json:"spotChecks"`
}

func (j spotJobJSON) all() []spotTaskJSON {
	if j.Reduce != nil {
		return append(append([]spotTaskJSON{}, j.Tasks...), *j.Reduce)
	}
	return j.Tasks
}

func putJSON(t *testing.T, url string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func setSpotCheck(t *testing.T, api string, percent int) {
	t.Helper()
	if code, raw := putJSON(t, api+"/policy", map[string]any{"allowUnlisted": true, "spotCheckPercent": percent}); code != http.StatusOK {
		t.Fatalf("PUT /policy: %d %s", code, raw)
	}
}

func setAvailability(t *testing.T, api string, id domain.NodeID, mode string) {
	t.Helper()
	if code, raw := putJSON(t, api+"/nodes/"+string(id)+"/availability", map[string]any{"mode": mode}); code != http.StatusOK {
		t.Fatalf("PUT availability %s: %d %s", mode, code, raw)
	}
}

// waitSpotChecks waits for the job to finish and for n of its tasks to
// have a settled spot check.
func waitSpotChecks(t *testing.T, api, id string, n int, timeout time.Duration) spotJobJSON {
	t.Helper()
	var j spotJobJSON
	waitFor(t, timeout, func() bool {
		j = spotJobJSON{}
		getJSON(t, api+"/jobs/"+id, &j)
		if j.State == domain.JobRunning {
			return false
		}
		settled := 0
		for _, task := range j.all() {
			if task.SpotCheck != "" && !strings.HasPrefix(task.SpotCheck, "checking") {
				settled++
			}
		}
		return settled >= n
	})
	return j
}

func countJobRequest(t *testing.T, api string, texts []string, target domain.NodeID, reduce bool) map[string]any {
	t.Helper()
	var tasks []map[string]any
	for i, text := range texts {
		sha := uploadArtifact(t, api, []byte(text))
		task := map[string]any{"capability": "text.count", "inputs": []map[string]string{{"name": "doc" + string(rune('a'+i)) + ".txt", "sha256": sha}}}
		if target != "" {
			task["target"] = target
		}
		tasks = append(tasks, task)
	}
	body := map[string]any{"name": "count", "tasks": tasks}
	if reduce {
		body["reduce"] = map[string]any{"capability": "file.hash"}
	}
	return body
}

func submitSpotJob(t *testing.T, api string, body map[string]any) string {
	t.Helper()
	code, job, raw := postJob(t, api, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	return job.ID
}

func mismatchEntries(t *testing.T, api string) []domain.AuditEntry {
	t.Helper()
	var all []domain.AuditEntry
	getJSON(t, api+"/audit?log=security&limit=1000", &all)
	var out []domain.AuditEntry
	for _, e := range all {
		if e.Kind == "node.result-mismatch" {
			out = append(out, e)
		}
	}
	return out
}

type nodeSuspectJSON struct {
	NodeID  domain.NodeID       `json:"nodeId"`
	Suspect *domain.SuspectMark `json:"suspect"`
}

func suspects(t *testing.T, api string) map[domain.NodeID]domain.SuspectMark {
	t.Helper()
	var nodes []nodeSuspectJSON
	getJSON(t, api+"/nodes", &nodes)
	out := map[domain.NodeID]domain.SuspectMark{}
	for _, n := range nodes {
		if n.Suspect != nil {
			out[n.NodeID] = *n.Suspect
		}
	}
	return out
}

// liar is a hostile device: it registers with the typed task types,
// fetches its inputs like a real agent, and uploads a wrong result.
type liar struct {
	id      domain.NodeID
	mu      sync.Mutex
	assigns []protocol.WorkloadAssignPayload
	errs    []string // transfer failures (not t.Errorf: the goroutines may outlive the test)
}

func (l *liar) fail(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, fmt.Sprintf(format, args...))
}

func (l *liar) check(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) > 0 {
		t.Fatalf("the lying device couldn't do its part: %v", l.errs)
	}
}

func (l *liar) got() []protocol.WorkloadAssignPayload {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]protocol.WorkloadAssignPayload(nil), l.assigns...)
}

func startLiar(t *testing.T, ctx context.Context, addr, name string) *liar {
	t.Helper()
	id, closer := registerRawNodeWithManifest(t, addr, name, func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: "text.count", Version: "1"}, {Name: "file.hash", Version: "1"}}
		mf.AgentFeatures = []string{domain.FeatureArtifacts, domain.FeatureTimeout}
		mf.WorkloadSlots = 4
	})
	t.Cleanup(func() { closer.Close() })
	conn := closer.(domain.Conn)
	l := &liar{id: id}
	var sendMu sync.Mutex
	send := func(typ protocol.MessageType, payload any) {
		env, _ := protocol.NewEnvelope(typ, id, domain.ManagerNodeID, payload)
		wire, _ := protocol.Encode(env)
		sendMu.Lock()
		defer sendMu.Unlock()
		conn.Send(ctx, wire)
	}
	go func() {
		for ctx.Err() == nil {
			send(protocol.MsgHeartbeat, protocol.HeartbeatPayload{RuntimeState: domain.RuntimeState{MemoryAvailableBytes: 8 << 30, LastHeartbeat: time.Now()}})
			time.Sleep(300 * time.Millisecond)
		}
	}()
	base := "http://" + addr + "/workload-artifacts/"
	transfer := func(method, url, token string, body []byte, sha string) (int, []byte) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if sha != "" {
			req.Header.Set("X-Artifact-SHA256", sha)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	work := func(p protocol.WorkloadAssignPayload) {
		w := p.Workload
		now := time.Now().UTC()
		send(protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{ID: w.ID, Target: id, State: domain.WorkloadRunning, StartedAt: now}})
		for _, in := range w.Inputs {
			if code, _ := transfer(http.MethodGet, base+string(w.ID)+"/inputs/"+in.SHA256, p.ArtifactToken, nil, ""); code != http.StatusOK {
				l.fail("fetch input %s: %d", in.Name, code)
			}
		}
		var claims []domain.ArtifactRef
		for _, name := range w.Outputs {
			wrong := []byte(`{"files":[],"total":{"lines":42,"words":42,"bytes":42}}` + "\n")
			sum := sha256.Sum256(wrong)
			sha := hex.EncodeToString(sum[:])
			if code, b := transfer(http.MethodPut, base+string(w.ID)+"/outputs/"+name, p.ArtifactToken, wrong, sha); code != http.StatusCreated {
				l.fail("upload %s: %d %s", name, code, b)
			}
			claims = append(claims, domain.ArtifactRef{Name: name, SHA256: sha, Size: int64(len(wrong))})
		}
		send(protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{
			ID: w.ID, Target: id, State: domain.WorkloadCompleted, StartedAt: now, FinishedAt: time.Now().UTC(), Outputs: claims,
		}})
	}
	go func() {
		for {
			data, err := conn.Receive(ctx)
			if err != nil {
				return
			}
			env, err := protocol.Decode(data)
			if err != nil || env.Type != protocol.MsgWorkloadAssign {
				continue
			}
			var p protocol.WorkloadAssignPayload
			if env.DecodePayload(&p) == nil {
				l.mu.Lock()
				l.assigns = append(l.assigns, p)
				l.mu.Unlock()
				go work(p)
			}
		}
	}()
	return l
}

func waitReadyWithHeartbeat(t *testing.T, srv *manager.Server, ids ...domain.NodeID) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		for _, id := range ids {
			rec, ok := srv.Registry.Get(id)
			if !ok || rec.State != domain.NodeReady || rec.LastMetrics.LastHeartbeat.IsZero() {
				return false
			}
		}
		return true
	})
}

// Honest devices agree: every checked task says "matched", each re-run
// went to the other device, and the job itself is untouched (one attempt
// per task, same outputs). With the policy off nothing extra runs.
func TestSpotCheckHonestDevicesMatchAndLeaveTheJobAlone(t *testing.T) {
	const addr = "127.0.0.1:19660"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "spot-honest-a", 2)
	b := startFileAgentWithSlots(t, ctx, m, addr, "spot-honest-b", 2)
	waitReadyWithHeartbeat(t, m.srv, a.NodeID(), b.NodeID())
	texts := []string{"one two three\n", "four five\nsix\n"}

	// Off (the default): no re-runs at all.
	off := submitSpotJob(t, m.api, countJobRequest(t, m.api, texts, "", true))
	if j := waitJob(t, m.api, off, 60*time.Second); j.State != domain.JobCompleted {
		t.Fatalf("job with checks off: %s %s", j.State, j.Error)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(m.srv.Workloads.List()); n != 3 {
		t.Fatalf("with spot checks off: %d workloads, want the 3 attempts only", n)
	}

	setSpotCheck(t, m.api, 100)
	id := submitSpotJob(t, m.api, countJobRequest(t, m.api, texts, "", true))
	j := waitSpotChecks(t, m.api, id, 3, 60*time.Second)
	if j.State != domain.JobCompleted {
		t.Fatalf("job %s: %s", j.State, j.Error)
	}
	originals := map[string]domain.NodeID{}
	for _, task := range j.all() {
		if !strings.HasPrefix(task.SpotCheck, "matched") || task.Attempts != 1 || task.State != "COMPLETED" {
			t.Fatalf("task %s: %+v", task.Key, task)
		}
		originals[task.Outputs[0].SHA256] = task.Node
	}
	if j.SpotChecks == nil || j.SpotChecks.Matched != 3 || j.SpotChecks.Mismatch+j.SpotChecks.Unresolved+j.SpotChecks.Skipped != 0 {
		t.Fatalf("job spot-check counts %+v", j.SpotChecks)
	}
	// The re-runs are plain workloads (no job), each on the device that
	// didn't produce the result it reproduced.
	checks := 0
	for _, rec := range m.srv.Workloads.List() {
		if rec.Workload.Job != "" || rec.Status.State != domain.WorkloadCompleted {
			continue
		}
		if rec.Workload.Attempt != 0 || len(rec.Status.Outputs) != 1 {
			t.Fatalf("check workload %+v", rec.Workload)
		}
		checks++
		orig, ok := originals[rec.Status.Outputs[0].SHA256]
		if !ok {
			t.Fatalf("check %s produced a result no task did", rec.Workload.ID)
		}
		if rec.Workload.Target == orig {
			t.Fatalf("check %s ran on %s, the device whose result it checked", rec.Workload.ID, orig)
		}
	}
	if checks != 3 {
		t.Fatalf("%d check workloads, want 3", checks)
	}
	again := waitSpotChecks(t, m.api, id, 3, 5*time.Second)
	if len(again.Outputs) != 1 || again.Outputs[0] != j.Outputs[0] {
		t.Fatalf("the job's result changed: %+v vs %+v", again.Outputs, j.Outputs)
	}
	if len(suspects(t, m.api)) != 0 || len(mismatchEntries(t, m.api)) != 0 {
		t.Fatal("honest devices must not be flagged")
	}
}

// A device that returns a wrong result on its own task is the odd one
// out once a third device breaks the tie: it alone is marked suspect,
// with one security-log entry for the job. The mark and the verdicts
// survive a manager restart, and only the operator clears the mark.
func TestSpotCheckFlagsALyingDevice(t *testing.T) {
	const addr = "127.0.0.1:19661"
	dbPath := filepath.Join(t.TempDir(), "spot.db")
	store := openArtifactStore(t, 1<<20)
	start := func() (*manager.Server, string, func()) {
		db, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		transport := ws.New()
		srv := manager.NewServer(transport, db, manager.Config{
			Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 10 * time.Second, ReconcileInterval: 100 * time.Millisecond, Artifacts: store,
		})
		transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
		go srv.Run(ctx)
		waitListening(t, addr)
		api := httptest.NewServer(srv.NewHTTPHandler())
		return srv, api.URL, func() { api.Close(); cancel(); time.Sleep(150 * time.Millisecond); db.Close() }
	}
	srv, api, stop := start()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	agentCtx, stopAgents := context.WithCancel(ctx)
	m := artifactManager{srv: srv, api: api}
	a := startFileAgentWithSlots(t, agentCtx, m, addr, "spot-flag-a", 2)
	b := startFileAgentWithSlots(t, agentCtx, m, addr, "spot-flag-b", 2)
	bad := startLiar(t, agentCtx, addr, "spot-flag-liar")
	waitReadyWithHeartbeat(t, srv, a.NodeID(), b.NodeID(), bad.id)
	setSpotCheck(t, api, 100)

	// Three tasks pinned to the liar: it lies on all three.
	id := submitSpotJob(t, api, countJobRequest(t, api, []string{"alpha beta\n", "gamma\n", "delta epsilon zeta\n"}, bad.id, false))
	j := waitSpotChecks(t, api, id, 3, 60*time.Second)
	if j.State != domain.JobCompleted {
		t.Fatalf("job %s: %s", j.State, j.Error)
	}
	for _, task := range j.Tasks {
		if task.Node != bad.id || task.Attempts != 1 {
			t.Fatalf("task %s ran %+v", task.Key, task)
		}
		if !strings.HasPrefix(task.SpotCheck, "mismatch: spot-flag-liar's result differed") {
			t.Fatalf("task %s spot check %q", task.Key, task.SpotCheck)
		}
	}
	bad.check(t)
	if j.SpotChecks == nil || j.SpotChecks.Mismatch != 3 {
		t.Fatalf("counts %+v", j.SpotChecks)
	}
	marks := suspects(t, api)
	if len(marks) != 1 || marks[bad.id].Count != 3 || marks[bad.id].Job != domain.JobID(id) {
		t.Fatalf("suspects %+v, want only the liar, 3 times", marks)
	}
	entries := mismatchEntries(t, api)
	if len(entries) != 1 || entries[0].NodeID != bad.id || entries[0].Log != domain.AuditSecurity {
		t.Fatalf("security log: %+v, want one node.result-mismatch for the liar", entries)
	}
	// The job's outputs are still the liar's: the check informs, it
	// doesn't rewrite results (the operator decides).
	if len(j.Tasks[0].Outputs) != 1 || downloadText(t, api, j.Tasks[0].Outputs[0].SHA256) == "" {
		t.Fatalf("outputs %+v", j.Tasks[0].Outputs)
	}

	stopAgents()
	stop()
	_, api2, stop2 := start()
	defer stop2()
	after := spotJobJSON{}
	getJSON(t, api2+"/jobs/"+id, &after)
	for _, task := range after.Tasks {
		if !strings.HasPrefix(task.SpotCheck, "mismatch:") {
			t.Fatalf("after a restart task %s says %q", task.Key, task.SpotCheck)
		}
	}
	if marks := suspects(t, api2); marks[bad.id].Count != 3 {
		t.Fatalf("after a restart the mark is %+v", marks)
	}
	del := func() int {
		req, _ := http.NewRequest(http.MethodDelete, api2+"/nodes/"+string(bad.id)+"/suspect", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := del(); code != http.StatusOK {
		t.Fatalf("clear: %d", code)
	}
	if code := del(); code != http.StatusNotFound {
		t.Fatalf("clear twice: %d, want 404", code)
	}
	if len(suspects(t, api2)) != 0 {
		t.Fatal("still marked after clearing")
	}
	var all []domain.AuditEntry
	getJSON(t, api2+"/audit?log=security", &all)
	cleared := false
	for _, e := range all {
		cleared = cleared || (e.Kind == "node.suspect-cleared" && e.NodeID == bad.id)
	}
	if !cleared {
		t.Fatal("clearing the mark must be audited")
	}
}

// A lying device that is handed someone else's result to check is caught
// by the tiebreak, and the honest device it contradicted is not marked.
// The device doing the check is told nothing that marks it as a check.
func TestSpotCheckTiebreakFlagsALyingChecker(t *testing.T) {
	const addr = "127.0.0.1:19662"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "spot-checker-a", 2)
	b := startFileAgentWithSlots(t, ctx, m, addr, "spot-checker-b", 2)
	bad := startLiar(t, ctx, addr, "spot-checker-liar")
	waitReadyWithHeartbeat(t, m.srv, a.NodeID(), b.NodeID(), bad.id)
	// b takes no work for now, so the only device left to check a's
	// result is the liar.
	setAvailability(t, m.api, b.NodeID(), "paused")
	setSpotCheck(t, m.api, 100)

	id := submitSpotJob(t, m.api, countJobRequest(t, m.api, []string{"the quick brown fox\n"}, a.NodeID(), false))
	waitFor(t, 30*time.Second, func() bool {
		var j spotJobJSON
		getJSON(t, m.api+"/jobs/"+id, &j)
		return len(j.Tasks) == 1 && strings.Contains(j.Tasks[0].SpotCheck, "third device")
	})
	got := bad.got()
	if len(got) != 1 {
		t.Fatalf("the liar got %d assignments, want just the check", len(got))
	}
	if w := got[0].Workload; w.Job != "" || w.Task != "" || w.Attempt != 0 || len(w.ExcludeNodes) != 0 || len(w.AvoidNodes) != 0 {
		t.Fatalf("the check's assignment gives it away: %+v", w)
	}
	setAvailability(t, m.api, b.NodeID(), "always") // the tiebreak can run now
	j := waitSpotChecks(t, m.api, id, 1, 30*time.Second)
	if task := j.Tasks[0]; task.Node != a.NodeID() || !strings.HasPrefix(task.SpotCheck, "mismatch: spot-checker-liar's result differed") {
		t.Fatalf("task %+v", task)
	}
	bad.check(t)
	marks := suspects(t, m.api)
	if len(marks) != 1 || marks[bad.id].Count != 1 {
		t.Fatalf("suspects %+v, want only the lying checker", marks)
	}
	if e := mismatchEntries(t, m.api); len(e) != 1 || e[0].NodeID != bad.id {
		t.Fatalf("security log %+v", e)
	}
}

// With only two devices a disagreement can't be settled: it is reported
// (job view and security log), but nobody is marked.
func TestSpotCheckTwoDevicesDisagreeUnresolved(t *testing.T) {
	const addr = "127.0.0.1:19663"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "spot-pair-a", 2)
	bad := startLiar(t, ctx, addr, "spot-pair-liar")
	waitReadyWithHeartbeat(t, m.srv, a.NodeID(), bad.id)
	setSpotCheck(t, m.api, 100)

	id := submitSpotJob(t, m.api, countJobRequest(t, m.api, []string{"x y\n", "z\n"}, bad.id, false))
	j := waitSpotChecks(t, m.api, id, 2, 60*time.Second)
	for _, task := range j.Tasks {
		if !strings.HasPrefix(task.SpotCheck, "mismatch, unresolved") || !strings.Contains(task.SpotCheck, "no third device") {
			t.Fatalf("task %s: %q", task.Key, task.SpotCheck)
		}
	}
	bad.check(t)
	if j.SpotChecks == nil || j.SpotChecks.Unresolved != 2 {
		t.Fatalf("counts %+v", j.SpotChecks)
	}
	if marks := suspects(t, m.api); len(marks) != 0 {
		t.Fatalf("an unresolved disagreement must not mark anyone: %+v", marks)
	}
	e := mismatchEntries(t, m.api)
	if len(e) != 1 || e[0].NodeID != "" {
		t.Fatalf("security log %+v, want one entry naming no single device", e)
	}
	nodes, _ := e[0].Detail["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("the entry must name both devices: %+v", e[0].Detail)
	}
}

// With a single device there is nothing to compare against: the job view
// says so, and nothing is left waiting.
func TestSpotCheckSkippedWithoutAnotherDevice(t *testing.T) {
	const addr = "127.0.0.1:19664"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "spot-alone", 2)
	waitReadyWithHeartbeat(t, m.srv, a.NodeID())
	setSpotCheck(t, m.api, 100)

	id := submitSpotJob(t, m.api, countJobRequest(t, m.api, []string{"solo\n", "again\n"}, "", true))
	j := waitSpotChecks(t, m.api, id, 3, 60*time.Second)
	if j.State != domain.JobCompleted {
		t.Fatalf("job %s: %s", j.State, j.Error)
	}
	for _, task := range j.all() {
		if task.SpotCheck != "skipped: no other device" {
			t.Fatalf("task %s: %q", task.Key, task.SpotCheck)
		}
	}
	if n := len(m.srv.Workloads.List()); n != 3 {
		t.Fatalf("%d workloads, want the 3 attempts only", n)
	}
}
