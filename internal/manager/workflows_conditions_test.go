package manager

import (
	"strings"
	"testing"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
)

func failAttempts(s *Server, job domain.JobID) {
	for _, attempts := range s.Workloads.byJob()[job] {
		for _, a := range attempts {
			switch a.Status.State {
			case domain.WorkloadPending, domain.WorkloadQueued, domain.WorkloadRunning:
				s.Workloads.UpdateStatus(domain.WorkloadStatus{ID: a.Workload.ID, Target: a.Workload.Target, State: domain.WorkloadFailed, Error: "boom"})
			}
		}
	}
}

func lastJob(s *Server, wf domain.WorkflowID, stage int) domain.Job {
	jobs := workflowJobs(s, wf)[stage]
	return jobs[len(jobs)-1]
}

func photoFiles(t *testing.T, store *artifacts.Store) []domain.ArtifactRef {
	return []domain.ArtifactRef{putFile(t, store, "a.jpg", "a"), putFile(t, store, "b.jpg", "b")}
}

func withStage(st domain.WorkflowStage, f func(*domain.WorkflowStage)) domain.WorkflowStage {
	f(&st)
	return st
}

// A stage whose condition is false is skipped (with the reason), and the
// stage after it works on the last stage that completed.
func TestWorkflowSkipsAStageAndPassesTheLastResultsOn(t *testing.T) {
	s, store := workflowTestServer(t)
	resize := stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"})
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: photoFiles(t, store), Stages: []domain.WorkflowStage{
		resize,
		withStage(stage("file.hash", "", nil), func(st *domain.WorkflowStage) { st.When = `previous.outputs.any(name matches "*.gif")` }),
		stage("archive.zip", "", map[string]string{"name": "out.zip"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	completeAttempts(t, s, store, lastJob(s, wf.ID, 0).ID)
	for i := 0; i < 3; i++ {
		pass(s)
		if jobs := workflowJobs(s, wf.ID); len(jobs[1]) != 0 {
			t.Fatal("the skipped stage got a job")
		}
		if jobs := workflowJobs(s, wf.ID); len(jobs[2]) > 1 {
			t.Fatal("stage 3 started twice")
		}
	}
	zipJob := lastJob(s, wf.ID, 2)
	var names []string
	for _, in := range zipJob.Tasks[0].Inputs {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "a-80.jpg") || !strings.Contains(got, "b-80.jpg") {
		t.Fatalf("stage 3 inputs %q: want the resized photos (stage 2 was skipped)", got)
	}
	completeAttempts(t, s, store, zipJob.ID)
	pass(s)
	pass(s)
	got, _ := s.workflows.get(wf.ID)
	v := s.toWorkflowView(got)
	if got.State != domain.JobCompleted || v.Stages[1].State != stageSkipped || !strings.Contains(v.Stages[1].Reason, "condition not met") ||
		v.Stages[2].State != "COMPLETED" || len(v.Outputs) != 1 || v.Outputs[0].Name != "out.zip" {
		t.Fatalf("%s %q %+v", got.State, got.Error, v.Stages)
	}
}

// onFailure continue: the stage may fail, a later stage sees
// previous.failed, and with no completed stage before it gets the
// workflow's own files.
func TestWorkflowContinuesPastAFailedStage(t *testing.T) {
	s, store := workflowTestServer(t)
	files := photoFiles(t, store)
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: files, MaxAttempts: 1, Stages: []domain.WorkflowStage{
		withStage(stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"}), func(st *domain.WorkflowStage) { st.OnFailure = "continue" }),
		withStage(stage("file.hash", "", nil), func(st *domain.WorkflowStage) { st.When = "previous.failed" }),
		withStage(stage("archive.zip", "", nil), func(st *domain.WorkflowStage) { st.When = "stage[1].succeeded" }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	failAttempts(s, lastJob(s, wf.ID, 0).ID)
	pass(s)
	pass(s)
	hash := lastJob(s, wf.ID, 1)
	if len(hash.Tasks[0].Inputs) != 2 || hash.Tasks[0].Inputs[0].Name != "a.jpg" {
		t.Fatalf("the stage after a failed one should get the workflow's files: %+v", hash.Tasks[0].Inputs)
	}
	completeAttempts(t, s, store, hash.ID)
	pass(s)
	pass(s)
	got, _ := s.workflows.get(wf.ID)
	v := s.toWorkflowView(got)
	if got.State != domain.JobCompleted || v.Stages[0].State != "FAILED" || v.Stages[1].State != "COMPLETED" || v.Stages[2].State != stageSkipped {
		t.Fatalf("%s %q %+v", got.State, got.Error, v.Stages)
	}
	if len(workflowJobs(s, wf.ID)[2]) != 0 {
		t.Fatal("stage 3 ran")
	}
}

// retry:2 runs a failed stage again, as a new job, at most twice.
func TestWorkflowRetriesAStage(t *testing.T) {
	s, store := workflowTestServer(t)
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: photoFiles(t, store), MaxAttempts: 1, Stages: []domain.WorkflowStage{
		withStage(stage("file.hash", "", nil), func(st *domain.WorkflowStage) { st.OnFailure = "retry:2" }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 3; run++ {
		pass(s)
		pass(s)
		if n := len(workflowJobs(s, wf.ID)[0]); n != run {
			t.Fatalf("after run %d there are %d jobs", run, n)
		}
		if run < 3 {
			failAttempts(s, lastJob(s, wf.ID, 0).ID)
			pass(s)
		}
	}
	failAttempts(s, lastJob(s, wf.ID, 0).ID)
	for i := 0; i < 3; i++ {
		pass(s)
	}
	got, _ := s.workflows.get(wf.ID)
	if got.State != domain.JobFailed || len(workflowJobs(s, wf.ID)[0]) != 3 {
		t.Fatalf("%s %q with %d jobs", got.State, got.Error, len(workflowJobs(s, wf.ID)[0]))
	}
}

// A stage that fails at submit time under continue is recorded and the
// workflow goes on; under the default it fails the workflow.
func TestWorkflowToleratesAStageThatCannotStart(t *testing.T) {
	s, store := workflowTestServer(t)
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: photoFiles(t, store), MaxAttempts: 1, Stages: []domain.WorkflowStage{
		stage("file.hash", "", nil),
		withStage(stage("archive.zip", "", nil), func(st *domain.WorkflowStage) { st.OnFailure = "continue" }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	p := domain.PermissivePolicy()
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"archive.zip": {Enabled: false}}
	s.policy = &policyStore{}
	s.policy.set(p)
	completeAttempts(t, s, store, lastJob(s, wf.ID, 0).ID)
	for i := 0; i < 3; i++ {
		pass(s)
	}
	got, _ := s.workflows.get(wf.ID)
	v := s.toWorkflowView(got)
	if got.State != domain.JobCompleted || v.Stages[1].State != "FAILED" || !strings.Contains(v.Stages[1].Reason, "disabled") || len(workflowJobs(s, wf.ID)[1]) != 0 {
		t.Fatalf("%s %q %+v", got.State, got.Error, v.Stages[1])
	}
}

func TestWorkflowStageSpecIsValidatedUpFront(t *testing.T) {
	s, store := workflowTestServer(t)
	files := photoFiles(t, store)
	hash := func(f func(*domain.WorkflowStage)) domain.WorkflowStage { return withStage(stage("file.hash", "", nil), f) }
	cases := map[string]struct {
		stages []domain.WorkflowStage
		want   string
	}{
		"bad condition":          {[]domain.WorkflowStage{stage("file.hash", "", nil), hash(func(st *domain.WorkflowStage) { st.When = "previous.nope" })}, `no "nope"`},
		"first step has previous": {[]domain.WorkflowStage{hash(func(st *domain.WorkflowStage) { st.When = "previous.failed" })}, "no previous step"},
		"later step":              {[]domain.WorkflowStage{stage("file.hash", "", nil), hash(func(st *domain.WorkflowStage) { st.When = "stage[2].succeeded" })}, "steps 1-1"},
		"bad onFailure":           {[]domain.WorkflowStage{hash(func(st *domain.WorkflowStage) { st.OnFailure = "ignore" })}, "isn't stop, continue or retry"},
		"retry too many":          {[]domain.WorkflowStage{hash(func(st *domain.WorkflowStage) { st.OnFailure = "retry:9" })}, "retry:N takes N"},
		"timeout":                 {[]domain.WorkflowStage{hash(func(st *domain.WorkflowStage) { st.TimeoutSeconds = -1 })}, "timeoutSeconds"},
		"attempts":                {[]domain.WorkflowStage{hash(func(st *domain.WorkflowStage) { st.MaxAttempts = 99 })}, "maxAttempts"},
		"stage after optional one can't take its files": {[]domain.WorkflowStage{
			stage("image.resize", domain.StagePerFile, map[string]string{"width": "80"}),
			hash(func(st *domain.WorkflowStage) { st.When = "true" }),
			stage("image.stack", "", nil)}, "accepts"},
	}
	for name, c := range cases {
		_, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: files, Stages: c.stages})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A step after a skippable one that makes no files has nothing to work on.
	_, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Stages: []domain.WorkflowStage{
		withStage(stage("cpu.burn", "", nil), func(st *domain.WorkflowStage) { st.When = "true" }), stage("file.hash", "", nil)}})
	if err == nil || !strings.Contains(err.Error(), "makes no files") {
		t.Errorf("after a stage with no files: %v", err)
	}
	// And a good one passes.
	if _, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: files, Stages: []domain.WorkflowStage{
		hash(func(st *domain.WorkflowStage) { st.When = "inputs.any(name matches \"*.jpg\")"; st.OnFailure = "retry:1"; st.TimeoutSeconds = 60; st.MaxAttempts = 2 })}}); err != nil {
		t.Fatal(err)
	}
}

// A stage's timeoutSeconds reaches the attempts, never above the policy's.
func TestWorkflowStageTimeoutReachesTheAttempts(t *testing.T) {
	s, store := workflowTestServer(t)
	wf, err := s.SubmitWorkflow(t.Context(), WorkflowSpec{Inputs: photoFiles(t, store), Stages: []domain.WorkflowStage{
		withStage(stage("file.hash", "", nil), func(st *domain.WorkflowStage) { st.TimeoutSeconds = 45 })}})
	if err != nil {
		t.Fatal(err)
	}
	pass(s)
	pass(s)
	for _, attempts := range s.Workloads.byJob()[lastJob(s, wf.ID, 0).ID] {
		if got := attempts[0].Workload.TimeoutSeconds; got != 45 {
			t.Fatalf("timeout %d, want 45", got)
		}
	}
}
