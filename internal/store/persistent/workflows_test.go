package persistent

import (
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
)

func TestWorkflowsPersistAcrossReopenAndStageJobsKeepTheirLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wf := domain.Workflow{ID: "wf1", Name: "photos", State: domain.JobRunning, MaxAttempts: 3,
		Inputs: []domain.ArtifactRef{{Name: "a.jpg", SHA256: "aa"}},
		Stages: []domain.WorkflowStage{
			{WorkflowStep: domain.WorkflowStep{Type: "image.resize", Params: map[string]string{"width": "800"}}, Mode: domain.StagePerFile,
				Combine: &domain.WorkflowStep{Type: "archive.zip"}},
			{WorkflowStep: domain.WorkflowStep{Type: "file.hash"}, Mode: domain.StageOnce},
		}}
	if err := s.UpsertWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	wf.State, wf.Error = domain.JobFailed, "step 2: boom"
	if err := s.UpsertWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(domain.Job{ID: "j1", State: domain.JobRunning, Workflow: "wf1", Stage: 1}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wfs, err := s.ListWorkflows()
	if err != nil || len(wfs) != 1 {
		t.Fatalf("ListWorkflows: %v %+v", err, wfs)
	}
	got := wfs[0]
	if got.State != domain.JobFailed || got.Error != "step 2: boom" || len(got.Stages) != 2 || got.Stages[0].Type != "image.resize" ||
		got.Stages[0].Params["width"] != "800" || got.Stages[0].Combine == nil || got.Stages[0].Combine.Type != "archive.zip" || got.Stages[1].Mode != domain.StageOnce {
		t.Fatalf("workflow after reopen %+v", got)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 || jobs[0].Workflow != "wf1" || jobs[0].Stage != 1 {
		t.Fatalf("stage job after reopen %+v", jobs)
	}
}
