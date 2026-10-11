package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// A condition skips a stage on the real fleet, and the stage after it
// works on the last stage that completed.
func TestWorkflowConditionalStageSkipsOnRealAgents(t *testing.T) {
	const addr = "127.0.0.1:19751"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	waitTwoFileAgents(t, ctx, m, addr, "cond")
	files := uploadPhotos(t, "", m.api, "a.png", "b.png")
	code, wf, raw := postWorkflow(t, "", m.api, map[string]any{"inputs": files, "stages": []map[string]any{
		{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "40"}},
		{"type": "file.hash", "when": `previous.outputs.any(name matches "*.gif")`},
		{"type": "archive.zip", "params": map[string]string{"name": "small.zip"}, "when": "stage[1].succeeded and not previous.succeeded"},
	}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workflows: %d %s", code, raw)
	}
	wf = waitWorkflow(t, "", m.api, wf.ID, 40*time.Second)
	if wf.State != domain.JobCompleted || len(wf.Outputs) != 1 || wf.Outputs[0].Name != "small.zip" {
		t.Fatalf("%s (%s) %+v", wf.State, wf.Error, wf.Outputs)
	}
	if wf.Stages[1].State != "SKIPPED" || wf.Stages[2].State != "COMPLETED" || wf.Stages[1].Job != "" {
		t.Fatalf("stage states %+v", wf.Stages)
	}
	_, zipBody := callAPI(t, "", http.MethodGet, m.api+"/artifacts/"+wf.Outputs[0].SHA256, nil)
	if got := zipNames(t, zipBody); got != "a-40.jpg,b-40.jpg" {
		t.Fatalf("zip holds %s (the resized photos: step 2 was skipped)", got)
	}
}

// Chaos: the manager is stopped and restarted in the middle of a
// scheduled two-step workflow, and again between ticks. Every schedule
// slot ran exactly once and every stage of every run has exactly one job.
func TestWorkflowScheduledRunsSurviveManagerRestarts(t *testing.T) {
	const addr = "127.0.0.1:19752"
	dbPath := filepath.Join(t.TempDir(), "schedules.db")
	store := openArtifactStore(t, 1<<20)
	var clock atomic.Int64
	base := time.Date(2026, 6, 1, 12, 0, 30, 0, time.UTC)
	clock.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
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
		srv.SetScheduleClock(now)
		transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
		go srv.Run(ctx)
		waitListening(t, addr)
		api := httptest.NewServer(srv.NewHTTPHandler())
		return srv, api.URL, func() { api.Close(); cancel(); time.Sleep(200 * time.Millisecond); db.Close() }
	}
	srv1, api1, stop1 := start()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := startFileAgentWithSlots(t, ctx, artifactManager{srv: srv1, api: api1}, addr, "sched-chaos", 1)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv1.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	files := uploadPhotos(t, "", api1, "a.png", "b.png")
	code, raw := callAPI(t, "", http.MethodPost, api1+"/schedules", map[string]any{
		"name": "chaos", "schedule": "* * * * *", "overlap": "allow", "maxRuns": 2,
		"workflow": map[string]any{"inputs": files, "stages": []map[string]any{
			{"type": "image.resize", "mode": "perFile", "params": map[string]string{"width": "40"}},
			{"type": "archive.zip", "params": map[string]string{"name": "small.zip"}},
		}},
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /schedules: %d %s", code, raw)
	}
	var sc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}

	// Hold the only slot so the first run's step 1 is mid-flight at the restart.
	if c, out := postWorkload(t, api1, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "4", "threads": "1"}}); c != http.StatusAccepted {
		t.Fatalf("hold: %d %v", c, out)
	}
	clock.Store(base.Add(45 * time.Second).UnixNano()) // slot 12:01:00
	waitFor(t, 10*time.Second, func() bool {
		var wfs []struct{ ID string }
		getJSON(t, api1+"/workflows", &wfs)
		return len(wfs) == 1
	})
	stop1() // mid-stage, with the schedule record already advanced
	_, api2, stop2 := start()
	// The same slot again after the restart must not run twice.
	time.Sleep(1500 * time.Millisecond)
	var wfs []struct {
		ID    string
		State domain.JobState
	}
	getJSON(t, api2+"/workflows", &wfs)
	if len(wfs) != 1 {
		t.Fatalf("after the restart there are %d workflows for one slot", len(wfs))
	}
	waitFor(t, 60*time.Second, func() bool {
		getJSON(t, api2+"/workflows", &wfs)
		return len(wfs) == 1 && wfs[0].State == domain.JobCompleted
	})
	stop2()
	// Another restart between ticks, then slot 12:02:00.
	_, api3, stop3 := start()
	defer stop3()
	clock.Store(base.Add(105 * time.Second).UnixNano())
	waitFor(t, 10*time.Second, func() bool {
		getJSON(t, api3+"/workflows", &wfs)
		return len(wfs) == 2
	})
	waitFor(t, 60*time.Second, func() bool {
		getJSON(t, api3+"/workflows", &wfs)
		return len(wfs) == 2 && wfs[0].State == domain.JobCompleted && wfs[1].State == domain.JobCompleted
	})
	clock.Store(base.Add(10 * time.Minute).UnixNano()) // maxRuns 2: nothing more
	time.Sleep(1200 * time.Millisecond)
	getJSON(t, api3+"/workflows", &wfs)
	var jobs []struct{ Name string }
	getJSON(t, api3+"/jobs", &jobs)
	if len(wfs) != 2 || len(jobs) != 4 {
		t.Fatalf("expected 2 workflows and 4 stage jobs, got %d and %d", len(wfs), len(jobs))
	}
	var got struct {
		State domain.ScheduleState `json:"state"`
		Runs  int                  `json:"runs"`
		Hist  []struct {
			Slot, Outcome string
		} `json:"history"`
	}
	getJSON(t, api3+"/schedules/"+sc.ID, &got)
	if got.State != domain.ScheduleEnded || got.Runs != 2 || len(got.Hist) != 2 || got.Hist[0].Slot == got.Hist[1].Slot {
		t.Fatalf("schedule: %+v", got)
	}
}
