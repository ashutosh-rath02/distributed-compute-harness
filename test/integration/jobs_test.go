package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

type jobTaskJSON struct {
	Key      string               `json:"key"`
	State    string               `json:"state"`
	Attempts int                  `json:"attempts"`
	Node     domain.NodeID        `json:"node"`
	Outputs  []domain.ArtifactRef `json:"outputs"`
	Error    string               `json:"error"`
	Waiting  string               `json:"waiting"`
}

type jobJSON struct {
	ID     string          `json:"id"`
	State  domain.JobState `json:"state"`
	Error  string          `json:"error"`
	Counts struct {
		Total, Completed, Active, Waiting, Failed, Canceled int
	} `json:"counts"`
	Tasks   []jobTaskJSON        `json:"tasks"`
	Reduce  *jobTaskJSON         `json:"reduce"`
	Outputs []domain.ArtifactRef `json:"outputs"`
}

func postJob(t *testing.T, api string, body map[string]any) (int, jobJSON, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(api+"/jobs", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var j jobJSON
	json.Unmarshal(raw, &j)
	return resp.StatusCode, j, string(raw)
}

func getJob(t *testing.T, api, id string) jobJSON {
	t.Helper()
	var j jobJSON
	getJSON(t, api+"/jobs/"+id, &j)
	return j
}

func waitJob(t *testing.T, api, id string, timeout time.Duration) jobJSON {
	t.Helper()
	var j jobJSON
	waitFor(t, timeout, func() bool { j = getJob(t, api, id); return j.State != domain.JobRunning })
	return j
}

// upperCommand upper-cases in.txt into out.txt; concatCommand joins
// every part's out.txt, in task order, into result.txt.
func upperCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", "(Get-Content in.txt).ToUpper() | Set-Content out.txt"}
	}
	return "sh", []string{"-c", "tr a-z A-Z < in.txt > out.txt"}
}

func concatCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", "Get-ChildItem parts -Recurse -File | Sort-Object FullName | Get-Content | Set-Content result.txt"}
	}
	return "sh", []string{"-c", "cat parts/*/out.txt > result.txt"}
}

func downloadText(t *testing.T, api, sha string) string {
	t.Helper()
	resp, err := http.Get(api + "/artifacts/" + sha)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.ReplaceAll(string(b), "\r", "")
}

// The headline case: one request, one task per input file, spread over
// the fleet, and a reduce that gets every task's output.
func TestMapJobSpreadsAcrossNodesAndReduces(t *testing.T) {
	const addr = "127.0.0.1:19540"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, m, addr, "job-agent-a", 2) // 6 tasks, 2 slots each: must spread
	b := startFileAgentWithSlots(t, ctx, m, addr, "job-agent-b", 2)
	waitFor(t, 5*time.Second, func() bool {
		ra, okA := m.srv.Registry.Get(a.NodeID())
		rb, okB := m.srv.Registry.Get(b.NodeID())
		return okA && okB && ra.State == domain.NodeReady && rb.State == domain.NodeReady
	})

	cmd, args := upperCommand()
	var tasks []map[string]any
	for i := 0; i < 6; i++ {
		sha := uploadArtifact(t, m.api, []byte("item-"+string(rune('0'+i))+"\n"))
		tasks = append(tasks, map[string]any{
			"name": "file-" + string(rune('0'+i)), "command": cmd, "args": args,
			"inputs": []map[string]string{{"name": "in.txt", "sha256": sha}}, "outputs": []string{"out.txt"},
		})
	}
	rcmd, rargs := concatCommand()
	code, job, raw := postJob(t, m.api, map[string]any{
		"name": "upper-all", "tasks": tasks,
		"reduce": map[string]any{"command": rcmd, "args": rargs, "outputs": []string{"result.txt"}},
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	job = waitJob(t, m.api, job.ID, 90*time.Second)
	if job.State != domain.JobCompleted {
		t.Fatalf("job %s: %s (%+v)", job.State, job.Error, job.Tasks)
	}
	nodes := map[domain.NodeID]int{}
	for _, task := range job.Tasks {
		nodes[task.Node]++
		if task.State != "COMPLETED" || len(task.Outputs) != 1 {
			t.Fatalf("task %s: %+v", task.Key, task)
		}
	}
	if len(nodes) < 2 {
		t.Fatalf("expected the tasks spread over both nodes, got %v", nodes)
	}
	if len(job.Outputs) != 1 || job.Outputs[0].Name != "result.txt" {
		t.Fatalf("job outputs %+v", job.Outputs)
	}
	if got := downloadText(t, m.api, job.Outputs[0].SHA256); got != "ITEM-0\nITEM-1\nITEM-2\nITEM-3\nITEM-4\nITEM-5\n" {
		t.Fatalf("reduce result %q", got)
	}
}

// A raw node that fails every task it gets, and advertises the most free
// memory so placement tries it first: the retry must go elsewhere.
func TestJobTaskRetryAvoidsTheNodeItFailedOn(t *testing.T) {
	const addr = "127.0.0.1:19541"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	badID, closer := registerRawNodeWithManifest(t, addr, "always-fails", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	send := func(typ protocol.MessageType, payload any) {
		env, _ := protocol.NewEnvelope(typ, badID, domain.ManagerNodeID, payload)
		wire, _ := protocol.Encode(env)
		conn.Send(ctx, wire)
	}
	send(protocol.MsgHeartbeat, protocol.HeartbeatPayload{RuntimeState: domain.RuntimeState{MemoryAvailableBytes: 1 << 40, LastHeartbeat: time.Now()}})
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
				now := time.Now().UTC()
				send(protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{
					ID: p.Workload.ID, Target: badID, State: domain.WorkloadFailed, Error: "this node always fails", ExitCode: 1, StartedAt: now, FinishedAt: now,
				}})
			}
		}
	}()
	good := startFileAgent(t, ctx, m, addr, "good-agent")
	waitFor(t, 5*time.Second, func() bool {
		rb, okB := m.srv.Registry.Get(badID)
		rg, okG := m.srv.Registry.Get(good.NodeID())
		return okB && okG && rb.State == domain.NodeReady && rg.State == domain.NodeReady && rb.LastMetrics.MemoryAvailableBytes > 0
	})

	cmd, args := echoArgs("ok")
	code, job, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{{"command": cmd, "args": args}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	job = waitJob(t, m.api, job.ID, 30*time.Second)
	if job.State != domain.JobCompleted || len(job.Tasks) != 1 {
		t.Fatalf("job %s: %s %+v", job.State, job.Error, job.Tasks)
	}
	if task := job.Tasks[0]; task.Attempts != 2 || task.Node != good.NodeID() {
		t.Fatalf("expected attempt 1 on the failing node and attempt 2 on %s, got %+v", good.NodeID(), task)
	}
}

// A task out of attempts fails the job; the other tasks still finish and
// keep their outputs; the reduce never runs.
func TestJobFailsWhenATaskRunsOutOfAttempts(t *testing.T) {
	const addr = "127.0.0.1:19542"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "exhaust-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	sha := uploadArtifact(t, m.api, []byte("fine\n"))
	ucmd, uargs := upperCommand()
	fcmd, fargs := failArgs()
	rcmd, rargs := concatCommand()
	code, job, raw := postJob(t, m.api, map[string]any{
		"maxAttempts": 2,
		"tasks": []map[string]any{
			{"command": ucmd, "args": uargs, "inputs": []map[string]string{{"name": "in.txt", "sha256": sha}}, "outputs": []string{"out.txt"}},
			{"command": fcmd, "args": fargs},
		},
		"reduce": map[string]any{"command": rcmd, "args": rargs, "outputs": []string{"result.txt"}},
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	job = waitJob(t, m.api, job.ID, 45*time.Second)
	if job.State != domain.JobFailed {
		t.Fatalf("job %s, want FAILED", job.State)
	}
	if job.Tasks[0].State != "COMPLETED" || len(job.Tasks[0].Outputs) != 1 {
		t.Fatalf("the healthy task must still finish with its output: %+v", job.Tasks[0])
	}
	if job.Tasks[1].State != "FAILED" || job.Tasks[1].Attempts != 2 {
		t.Fatalf("the failing task must use exactly maxAttempts (first run included): %+v", job.Tasks[1])
	}
	if job.Reduce == nil || job.Reduce.Attempts != 0 {
		t.Fatalf("the reduce must not run for a failed job: %+v", job.Reduce)
	}
}

// A manager restart in the middle of a job is not the task's fault: the
// interrupted attempt is retried without being charged (MaxAttempts 1
// would otherwise fail the job), and the job completes.
func TestJobResumesAfterManagerRestartWithoutChargingAnAttempt(t *testing.T) {
	const addr = "127.0.0.1:19543"
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
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
	a := startFileAgent(t, ctx, artifactManager{srv: srv1, api: api1}, addr, "restart-job-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := srv1.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	scmd, sargs := sleepArgs("4")
	code, job, raw := postJob(t, api1, map[string]any{"maxAttempts": 1, "tasks": []map[string]any{{"command": scmd, "args": sargs}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	waitFor(t, 10*time.Second, func() bool {
		j := getJob(t, api1, job.ID)
		return len(j.Tasks) == 1 && j.Tasks[0].State == "ACTIVE" && j.Tasks[0].Node != ""
	})
	time.Sleep(500 * time.Millisecond)
	stop1()

	_, api2, stop2 := start()
	defer stop2()
	j := waitJob(t, api2, job.ID, 45*time.Second)
	if j.State != domain.JobCompleted || j.Tasks[0].Attempts != 2 {
		t.Fatalf("after a manager restart: job %s (%s), task %+v; want COMPLETED on an uncharged 2nd attempt", j.State, j.Error, j.Tasks[0])
	}
}

func TestCancelJobStopsEveryAttempt(t *testing.T) {
	const addr = "127.0.0.1:19544"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "cancel-job-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	scmd, sargs := sleepArgs("30")
	var tasks []map[string]any
	for i := 0; i < 4; i++ {
		tasks = append(tasks, map[string]any{"command": scmd, "args": sargs})
	}
	_, job, raw := postJob(t, m.api, map[string]any{"tasks": tasks})
	if job.ID == "" {
		t.Fatalf("POST /jobs: %s", raw)
	}
	waitFor(t, 10*time.Second, func() bool { return getJob(t, m.api, job.ID).Counts.Active == 4 })
	resp, err := http.Post(m.api+"/jobs/"+job.ID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, 15*time.Second, func() bool { return getJob(t, m.api, job.ID).Counts.Canceled == 4 })
	j := getJob(t, m.api, job.ID)
	if j.State != domain.JobCanceled {
		t.Fatalf("job state %s, want CANCELED", j.State)
	}
	time.Sleep(500 * time.Millisecond) // several reconcile passes
	for _, task := range getJob(t, m.api, job.ID).Tasks {
		if task.Attempts != 1 || task.State != "CANCELED" {
			t.Fatalf("a canceled job must not retry: %+v", task)
		}
	}
}

func TestImpossibleJobsAreRefusedUpFront(t *testing.T) {
	const addr = "127.0.0.1:19545"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "refuse-job-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	cmd, args := echoArgs("x")
	many := make([]string, 0, 17)
	for i := 0; i < 17; i++ {
		many = append(many, "o"+string(rune('a'+i)))
	}
	tooManyParts := make([]map[string]any, 0, 20)
	for i := 0; i < 20; i++ {
		tooManyParts = append(tooManyParts, map[string]any{"command": cmd, "outputs": []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt", "g.txt", "h.txt", "i.txt", "j.txt", "k.txt", "l.txt", "m.txt"}})
	}
	for _, c := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no tasks", map[string]any{"tasks": []map[string]any{}}, 400},
		{"no command", map[string]any{"tasks": []map[string]any{{"args": args}}}, 400},
		{"too many attempts", map[string]any{"maxAttempts": 99, "tasks": []map[string]any{{"command": cmd}}}, 400},
		{"task no node could run", map[string]any{"tasks": []map[string]any{{"command": cmd, "requirements": map[string]any{"minMemoryBytes": uint64(1) << 50}}}}, 409},
		{"reduce parts too deep", map[string]any{"tasks": []map[string]any{{"command": cmd, "outputs": []string{"a/b/c.txt"}}}, "reduce": map[string]any{"command": cmd}}, 400},
		{"reduce over the input limit", map[string]any{"tasks": tooManyParts, "reduce": map[string]any{"command": cmd}}, 400},
	} {
		if code, _, raw := postJob(t, m.api, c.body); code != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, code, raw, c.want)
		}
	}
}

// A device that drops out mid-task isn't the task's fault either: the
// attempt is retried, uncharged, on whichever node is there next.
func TestJobRetriesUnchargedWhenItsNodeDisappears(t *testing.T) {
	const addr = "127.0.0.1:19546"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctxA, killA := context.WithCancel(ctx)
	a := startFileAgent(t, ctxA, m, addr, "vanishing-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	scmd, sargs := sleepArgs("3")
	_, job, raw := postJob(t, m.api, map[string]any{"maxAttempts": 1, "tasks": []map[string]any{{"command": scmd, "args": sargs}}})
	if job.ID == "" {
		t.Fatalf("POST /jobs: %s", raw)
	}
	waitFor(t, 10*time.Second, func() bool {
		j := getJob(t, m.api, job.ID)
		return j.Tasks[0].State == "ACTIVE" && j.Tasks[0].Node == a.NodeID()
	})
	killA()
	b := startFileAgent(t, ctx, m, addr, "replacement-agent")
	j := waitJob(t, m.api, job.ID, 40*time.Second)
	if j.State != domain.JobCompleted || j.Tasks[0].Attempts != 2 || j.Tasks[0].Node != b.NodeID() {
		t.Fatalf("job %s (%s), task %+v; want COMPLETED on %s by an uncharged 2nd attempt", j.State, j.Error, j.Tasks[0], b.NodeID())
	}
}
