package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// workflowTestServer is a manager with one READY device offering every
// catalog type, and an artifact store.
func workflowTestServer(t *testing.T) (*Server, *artifacts.Store) {
	t.Helper()
	store, err := artifacts.Open(artifacts.Config{Dir: t.TempDir(), MaxBytes: 1 << 20, TotalBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	var caps []domain.Capability
	for _, ty := range catalog.Types() {
		caps = append(caps, domain.Capability{Name: ty.Name, Version: ty.Version})
	}
	m := testNode("n1")
	m.Capabilities = caps
	m.AgentFeatures = []string{domain.FeatureArtifacts, domain.FeatureTimeout}
	r.Upsert(m, &fakeConn{tag: "n1"})
	r.SetState("n1", domain.NodeReady)
	r.UpdateResources("n1", []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 8, Unit: "cores"}}, caps)
	r.RecordHeartbeat("n1", domain.RuntimeState{MemoryAvailableBytes: 64 << 30, LastHeartbeat: time.Now()})
	r.mu.Lock()
	r.nodes["n1"].slotsN = 64
	r.mu.Unlock()
	s := newReconcileTestServer(r, NewWorkloadRegistry())
	s.cfg.Artifacts = store
	s.jobs, s.workflows, s.grants, s.dispatchKick = newJobTable(), newWorkflowTable(), newGrantTable(), make(chan struct{}, 1)
	return s, store
}

func putFile(t *testing.T, store *artifacts.Store, name, content string) domain.ArtifactRef {
	t.Helper()
	info, err := store.Put(strings.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	return domain.ArtifactRef{Name: name, SHA256: info.SHA256}
}

func stage(typ domain.CapabilityName, mode string, params map[string]string) domain.WorkflowStage {
	return domain.WorkflowStage{WorkflowStep: domain.WorkflowStep{Type: typ, Params: params}, Mode: mode}
}

// Several tasks' results go on as parts/<task>/<name>: never colliding,
// and in task order when sorted by name (image.stack's order) — bare
// names would put fractal-10.png before fractal-2.png.
func TestHandOnKeepsResultsApartAndInOrder(t *testing.T) {
	var tasks [][]domain.ArtifactRef
	for i := 0; i < 12; i++ {
		tasks = append(tasks, []domain.ArtifactRef{{Name: fmt.Sprintf("fractal-%d.png", i), SHA256: "s"}})
	}
	got := handOn(false, nil, tasks)
	names := make([]string, len(got))
	for i, f := range got {
		names[i] = f.Name
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != strings.Join(names, ",") || names[10] != "parts/0010/fractal-10.png" {
		t.Fatalf("results out of task order by name: %v", names)
	}
	if got := handOn(false, nil, [][]domain.ArtifactRef{{{Name: "a.zip"}}}); len(got) != 1 || got[0].Name != "a.zip" {
		t.Fatalf("one task's results keep their names: %v", got)
	}
	if got := handOn(true, []domain.ArtifactRef{{Name: "all.zip"}}, tasks); len(got) != 1 || got[0].Name != "all.zip" {
		t.Fatalf("a combine step's results are the stage's: %v", got)
	}
}

// Each stage is checked over the names the stage before will hand on.
func TestCheckWorkflowBuildsEachStageOverTheResultsBefore(t *testing.T) {
	s, store := workflowTestServer(t)
	var files []domain.ArtifactRef
	for _, n := range []string{"beach.jpg", "city.png", "dog.jpg"} {
		files = append(files, putFile(t, store, n, "image "+n))
	}
	spec := WorkflowSpec{Inputs: files, Stages: []domain.WorkflowStage{
		stage("image.resize", domain.StagePerFile, map[string]string{"width": "800"}),
		stage("archive.zip", "", map[string]string{"name": "photos.zip"}),
		stage("file.hash", domain.StageOnce, nil),
	}}
	jobs, err := s.checkWorkflow(&spec)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Stages[1].Mode != domain.StageOnce || spec.MaxAttempts != domain.DefaultJobRetries {
		t.Fatalf("not normalized: %+v", spec)
	}
	if len(jobs) != 3 || len(jobs[0].Tasks) != 3 || len(jobs[1].Tasks) != 1 || len(jobs[2].Tasks) != 1 {
		t.Fatalf("stage jobs %+v", jobs)
	}
	var zipIn []string
	for _, in := range jobs[1].Tasks[0].Inputs {
		zipIn = append(zipIn, in.Name)
	}
	if strings.Join(zipIn, ",") != "parts/0000/beach-800.jpg,parts/0001/city-800.jpg,parts/0002/dog-800.jpg" {
		t.Fatalf("zip step's files %v", zipIn)
	}
	if in := jobs[2].Tasks[0].Inputs; len(in) != 1 || in[0].Name != "photos.zip" || jobs[2].Tasks[0].Outputs[0] != "hashes.txt" {
		t.Fatalf("hash step %+v", jobs[2].Tasks[0])
	}
	// The stages' jobs carry what was checked, but nothing was recorded.
	if len(s.jobs.list()) != 0 || len(s.Workloads.List()) != 0 {
		t.Fatal("checking a workflow ran something")
	}

	// Strips, stacked, resized: the stack gets the strips in task order.
	spec = WorkflowSpec{Stages: []domain.WorkflowStage{
		{WorkflowStep: domain.WorkflowStep{Type: "render.fractal", Params: map[string]string{"width": "64", "height": "48"}}, Mode: domain.StageParts, Parts: 12},
		stage("image.stack", "", nil),
		stage("image.resize", domain.StagePerFile, map[string]string{"width": "32"}),
	}}
	if jobs, err = s.checkWorkflow(&spec); err != nil {
		t.Fatal(err)
	}
	if p := jobs[0].Tasks[11].Params; p["part"] != "11" || p["parts"] != "12" {
		t.Fatalf("part counters %v", p)
	}
	if in := jobs[1].Tasks[0].Inputs; len(in) != 12 || in[2].Name != "parts/0002/fractal-2.png" {
		t.Fatalf("stack step's files %v", in)
	}
	if out := jobs[2].Tasks[0].Outputs; len(out) != 1 || out[0] != "image-32.jpg" {
		t.Fatalf("resize step's outputs %v", out)
	}
}

// Anything that can't run is refused before anything runs.
func TestSubmitWorkflowRefusesWhatCantRun(t *testing.T) {
	s, store := workflowTestServer(t)
	photos := []domain.ArtifactRef{putFile(t, store, "a.jpg", "a"), putFile(t, store, "b.jpg", "b"), putFile(t, store, "c.jpg", "c")}
	resize := stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"})
	nine := make([]domain.WorkflowStage, 9)
	for i := range nine {
		nine[i] = stage("file.hash", "", nil)
	}
	for name, tc := range map[string]struct {
		spec WorkflowSpec
		want string
	}{
		"no steps":   {WorkflowSpec{}, "1-8 steps"},
		"nine steps": {WorkflowSpec{Inputs: photos, Stages: nine}, "1-8 steps"},
		"raw command": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{resize, stage(domain.CapabilitySystemExecute, "", nil)}},
			`step 2 (system.execute): manager: invalid workload: "system.execute" isn't a built-in task type`},
		"one device's own": {WorkflowSpec{Stages: []domain.WorkflowStage{stage("llm.pull", "", map[string]string{"model": "m"})}}, "one named device"},
		"unknown mode":     {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{stage("file.hash", "everywhere", nil)}}, "unknown mode"},
		"parts, not a parts type": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{{WorkflowStep: domain.WorkflowStep{Type: "file.hash"}, Mode: domain.StageParts, Parts: 2}}},
			"can't be split"},
		"parts given twice": {WorkflowSpec{Stages: []domain.WorkflowStage{{WorkflowStep: domain.WorkflowStep{Type: "render.fractal", Params: map[string]string{"parts": "4"}}, Mode: domain.StageParts, Parts: 2}}},
			"leave them out"},
		"one-file type given all at once": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{resize, stage("image.resize", domain.StageOnce, map[string]string{"width": "40"})}},
			"step 2 (Resize an image): task 0000: manager: invalid workload: catalog: invalid task: image.resize takes 1-1 input files, got 3"},
		"nothing handed on": {WorkflowSpec{Stages: []domain.WorkflowStage{stage("cpu.burn", "", nil), stage("file.hash", "", nil)}},
			"step 2 (Checksum files): manager: invalid workload: nothing to work on: step 1 makes no files"},
		"per file without files": {WorkflowSpec{Stages: []domain.WorkflowStage{stage("file.hash", domain.StagePerFile, nil)}}, "the workflow has no files"},
		"wrong kind of file": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{stage("file.hash", "", nil), stage("image.stack", "", nil)}},
			"image.stack accepts .png, .jpg, .jpeg, .gif files, not hashes.txt"},
		"result over its input": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{stage("file.hash", "", nil), stage("file.hash", "", nil)}},
			"step 2 (Checksum files): task 0000"},
		"too many results for one run": {WorkflowSpec{Stages: []domain.WorkflowStage{
			{WorkflowStep: domain.WorkflowStep{Type: "render.fractal"}, Mode: domain.StageParts, Parts: 300}, stage("image.stack", "", nil)}},
			"takes 1-256 input files, got 300"},
		"combine taking no files": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{{WorkflowStep: resize.WorkflowStep, Mode: domain.StagePerFile, Combine: &domain.WorkflowStep{Type: "cpu.burn"}}}},
			"can't combine results"},
		"a file not stored": {WorkflowSpec{Inputs: []domain.ArtifactRef{{Name: "x.jpg", SHA256: strings.Repeat("e", 64)}}, Stages: []domain.WorkflowStage{resize}}, "input x.jpg"},
		"a bad parameter late": {WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{resize, stage("archive.zip", "", map[string]string{"name": "../x.zip"})}},
			"step 2 (Zip files together): task 0000"},
	} {
		_, err := s.SubmitWorkflow(t.Context(), tc.spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, ErrInvalidWorkload) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}

	// A type disabled by policy, even at the last step: 403 territory.
	s.policy = &policyStore{p: domain.PermissivePolicy()}
	p := domain.PermissivePolicy()
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"file.hash": {Enabled: false}}
	s.policy.set(p)
	_, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: photos, Stages: []domain.WorkflowStage{resize, stage("archive.zip", "", nil), stage("file.hash", "", nil)}})
	if !errors.Is(err, ErrPolicy) || !strings.HasPrefix(err.Error(), "step 3 (Checksum files)") {
		t.Fatalf("disabled type at step 3: %v", err)
	}
	if len(s.workflows.list()) != 0 || len(s.jobs.list()) != 0 || len(s.Workloads.List()) != 0 {
		t.Fatal("a refused workflow left something behind")
	}
}

// completeAttempts finishes every in-flight attempt of job as an agent
// would: each declared output stored and delivered.
func completeAttempts(t *testing.T, s *Server, store *artifacts.Store, job domain.JobID) {
	t.Helper()
	for _, attempts := range s.Workloads.byJob()[job] {
		for _, a := range attempts {
			if a.Status.State != domain.WorkloadPending && a.Status.State != domain.WorkloadQueued && a.Status.State != domain.WorkloadRunning {
				continue
			}
			var outs []domain.ArtifactRef
			for _, name := range a.Workload.Outputs {
				f := putFile(t, store, name, "result "+name+" of "+string(a.Workload.ID))
				outs = append(outs, f)
			}
			s.Workloads.UpdateStatus(domain.WorkloadStatus{ID: a.Workload.ID, Target: a.Workload.Target, State: domain.WorkloadCompleted, Outputs: outs})
		}
	}
}

// pass is one reconcile pass, as the loop runs it.
func pass(s *Server) {
	s.advanceJobs(context.Background())
	s.advanceWorkflows(context.Background())
}

func workflowJobs(s *Server, id domain.WorkflowID) map[int][]domain.Job {
	out := map[int][]domain.Job{}
	for _, j := range s.jobs.list() {
		if j.Workflow == id {
			out[j.Stage] = append(out[j.Stage], j)
		}
	}
	return out
}

// Stages run in order, each on the results of the one before; a pass
// that finds a stage already submitted never submits it again; the last
// stage's results are the workflow's.
func TestWorkflowRunsItsStagesInOrder(t *testing.T) {
	s, store := workflowTestServer(t)
	files := []domain.ArtifactRef{putFile(t, store, "a.jpg", "a"), putFile(t, store, "b.jpg", "b")}
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Name: "photos", Inputs: files, Stages: []domain.WorkflowStage{
		{WorkflowStep: domain.WorkflowStep{Type: "image.resize", Params: map[string]string{"width": "80"}}, Mode: domain.StagePerFile,
			Combine: &domain.WorkflowStep{Type: "archive.zip", Params: map[string]string{"name": "small.zip"}}},
		stage("file.hash", "", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	jobs := workflowJobs(s, wf.ID)
	if len(jobs) != 1 || len(jobs[0]) != 1 {
		t.Fatalf("after two passes: %v", jobs)
	}
	if v := s.toWorkflowView(wf); v.Stage != 1 || v.Stages[0].State != "RUNNING" || v.Stages[1].State != stagePending {
		t.Fatalf("view %+v", v)
	}
	stage1 := jobs[0][0]
	completeAttempts(t, s, store, stage1.ID) // the two resizes
	pass(s)                                  // the zip starts
	completeAttempts(t, s, store, stage1.ID)
	pass(s) // stage 1 done: stage 2 starts over its zip
	jobs = workflowJobs(s, wf.ID)
	if len(jobs) != 2 || len(jobs[1]) != 1 {
		t.Fatalf("stage 2 not started once: %v", jobs)
	}
	stage2 := jobs[1][0]
	if in := stage2.Tasks[0].Inputs; len(in) != 1 || in[0].Name != "small.zip" {
		t.Fatalf("stage 2's files %+v", in)
	}
	// A restart in between: a fresh table derives the same stage.
	again := newWorkflowTable()
	for _, w := range s.workflows.list() {
		again.put(w)
	}
	s.workflows = again
	pass(s)
	if jobs = workflowJobs(s, wf.ID); len(jobs[0]) != 1 || len(jobs[1]) != 1 {
		t.Fatalf("a stage submitted twice: %v", jobs)
	}
	completeAttempts(t, s, store, stage2.ID)
	pass(s)
	pass(s)
	done, _ := s.workflows.get(wf.ID)
	v := s.toWorkflowView(done)
	if done.State != domain.JobCompleted || len(v.Outputs) != 1 || v.Outputs[0].Name != "hashes.txt" || v.Stage != 2 {
		t.Fatalf("finished: %s %q %+v", done.State, done.Error, v)
	}
	if v.Stages[0].Outputs[0].Name != "small.zip" || v.Stages[0].Counts.Completed != 2 {
		t.Fatalf("stage 1 view %+v", v.Stages[0])
	}
}

// A failed stage fails the workflow with the reason; so does a step whose
// type was disabled while the stage before it ran. Later stages never start.
func TestWorkflowFailsWithItsStage(t *testing.T) {
	s, store := workflowTestServer(t)
	files := []domain.ArtifactRef{putFile(t, store, "a.jpg", "a")}
	spec := func() WorkflowSpec {
		return WorkflowSpec{Inputs: files, MaxAttempts: 1, Stages: []domain.WorkflowStage{
			stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"}), stage("archive.zip", "", nil), stage("file.hash", "", nil)}}
	}
	wf, err := s.SubmitWorkflow(t.Context(), spec())
	if err != nil {
		t.Fatal(err)
	}
	pass(s) // the stage's job
	pass(s) // its attempts
	for _, attempts := range s.Workloads.byJob()[workflowJobs(s, wf.ID)[0][0].ID] {
		s.Workloads.UpdateStatus(domain.WorkloadStatus{ID: attempts[0].Workload.ID, Target: "n1", State: domain.WorkloadFailed, Error: "not an image"})
	}
	pass(s)
	pass(s)
	got, _ := s.workflows.get(wf.ID)
	if got.State != domain.JobFailed || !strings.HasPrefix(got.Error, "step 1 (Resize an image): 1 task(s) failed") || len(workflowJobs(s, wf.ID)) != 1 {
		t.Fatalf("failed stage: %s %q", got.State, got.Error)
	}
	if v := s.toWorkflowView(got); v.Stages[1].State != stageSkipped {
		t.Fatalf("view %+v", v.Stages[1])
	}

	wf, err = s.SubmitWorkflow(t.Context(), spec())
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	p := domain.PermissivePolicy()
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"archive.zip": {Enabled: false}}
	s.policy = &policyStore{}
	s.policy.set(p)
	completeAttempts(t, s, store, workflowJobs(s, wf.ID)[0][0].ID)
	pass(s)
	pass(s)
	got, _ = s.workflows.get(wf.ID)
	if got.State != domain.JobFailed || !strings.Contains(got.Error, "step 2 (Zip files together)") || !strings.Contains(got.Error, "archive.zip is disabled") {
		t.Fatalf("disabled meanwhile: %s %q", got.State, got.Error)
	}
	if len(workflowJobs(s, wf.ID)) != 1 {
		t.Fatal("a stage started after the workflow failed")
	}
}

// Cancel stops the running stage's job; a stage job made concurrently
// (or across a crash) is canceled by the next pass.
func TestCancelWorkflowStopsItsStage(t *testing.T) {
	s, store := workflowTestServer(t)
	files := []domain.ArtifactRef{putFile(t, store, "a.jpg", "a")}
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: files, Stages: []domain.WorkflowStage{
		stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"}), stage("archive.zip", "", nil)}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	if err := s.CancelWorkflow(t.Context(), wf.ID); err != nil {
		t.Fatal(err)
	}
	stage1 := workflowJobs(s, wf.ID)[0][0]
	if j, _ := s.jobs.get(stage1.ID); j.State != domain.JobCanceled {
		t.Fatalf("stage job %s", j.State)
	}
	// A stage job that slipped in after the cancel.
	late, err := s.SubmitJob(t.Context(), stageJob(wf, 1, []domain.ArtifactRef{putFile(t, store, "x.jpg", "x")}))
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	if j, _ := s.jobs.get(late.ID); j.State != domain.JobCanceled {
		t.Fatalf("late stage job %s", j.State)
	}
	if got, _ := s.workflows.get(wf.ID); got.State != domain.JobCanceled {
		t.Fatalf("workflow %s", got.State)
	}
	if err := s.CancelWorkflow(t.Context(), "nope"); !errors.Is(err, ErrUnknownWorkflow) {
		t.Fatalf("unknown: %v", err)
	}
}

// Between stages, nothing but the workflow holds the next stage's files.
func TestWorkflowResultsStayLiveBetweenStages(t *testing.T) {
	s, store := workflowTestServer(t)
	in := putFile(t, store, "a.jpg", "a")
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: []domain.ArtifactRef{in}, Stages: []domain.WorkflowStage{
		stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"}), stage("archive.zip", "", nil)}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	job := workflowJobs(s, wf.ID)[0][0]
	completeAttempts(t, s, store, job.ID)
	s.advanceJobs(t.Context()) // stage 1 done, stage 2 not started yet
	out := s.Workloads.byJob()[job.ID][domain.TaskKey(0)][0].Status.Outputs[0]
	live := s.liveArtifacts()
	if !live[out.SHA256] || !live[in.SHA256] {
		t.Fatalf("between stages: %v", live)
	}
	s.CancelWorkflow(t.Context(), wf.ID)
	if live := s.liveArtifacts(); live[out.SHA256] {
		t.Fatal("an ended workflow keeps its results live")
	}
}
