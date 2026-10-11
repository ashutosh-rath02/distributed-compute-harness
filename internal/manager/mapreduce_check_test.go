package manager

import (
	"testing"

	"home-harness/internal/domain"
)

// The map-reduce shape is checked up front like any workflow: the split's
// chunk names are known from its parameters, the map runs once per chunk,
// and the merge takes every chunk's counts.json.
func TestMapReduceWorkflowChecksUpFront(t *testing.T) {
	s, store := workflowTestServer(t)
	big := []domain.ArtifactRef{putFile(t, store, "big.txt", "a b\nc d\n")}
	split := stage("text.split", "", map[string]string{"chunks": "5"})
	count := stage("text.count", domain.StagePerFile, nil)
	count.Combine = &domain.WorkflowStep{Type: "json.merge", Params: map[string]string{"mode": "sum", "field": "total"}}
	spec := WorkflowSpec{Inputs: big, Stages: []domain.WorkflowStage{split, count}}
	jobs, err := s.checkWorkflow(&spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs[0].Tasks[0].Outputs) != 5 || jobs[0].Tasks[0].Outputs[0] != "chunk-0001.txt" || len(jobs[1].Tasks) != 5 || jobs[1].Reduce == nil {
		t.Fatalf("split outputs %v, map tasks %d", jobs[0].Tasks[0].Outputs, len(jobs[1].Tasks))
	}
	spec.Stages[0].Params["chunks"] = "17"
	if _, err := s.checkWorkflow(&spec); err == nil {
		t.Fatal("17 chunks accepted")
	}
}
