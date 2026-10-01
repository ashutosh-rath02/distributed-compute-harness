package manager

import (
	"net/http/httptest"
	"testing"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
)

func TestGrantTableMintReplacesAndDrops(t *testing.T) {
	g := newGrantTable()
	first, err := g.mint("w1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := g.mint("w1", "n2") // a re-dispatch: the old token dies
	if _, ok := g.lookup(first); ok {
		t.Fatal("a superseded assignment's token still works")
	}
	gr, ok := g.lookup(second)
	if !ok || gr.node != "n2" {
		t.Fatalf("lookup(second) = %+v, %v", gr, ok)
	}
	other, _ := g.mint("w2", "n1")
	g.sweep(func(id domain.WorkloadID, _ domain.NodeID) bool { return id == "w2" })
	if _, ok := g.lookup(second); ok || g.size() != 1 {
		t.Fatal("sweep kept a dead grant")
	}
	g.drop("w2")
	if _, ok := g.lookup(other); ok || g.size() != 0 {
		t.Fatal("drop kept the grant")
	}
}

func TestGrantUploadsOncePerOutputAndVerifies(t *testing.T) {
	g := newGrantTable()
	tok, _ := g.mint("w", "n")
	gr, _ := g.lookup(tok)
	const budget = 10
	if g.beginUpload(gr, "a", 1, budget) != nil || g.beginUpload(gr, "a", 1, budget) == nil {
		t.Fatal("a second concurrent upload of one output was allowed")
	}
	g.finishUpload(gr, "a", 1, nil) // failed: may retry, reservation returned
	if g.beginUpload(gr, "a", 1, budget) != nil {
		t.Fatal("a failed upload must not use up the output")
	}
	ref := domain.ArtifactRef{Name: "a", SHA256: "aa", Size: 1}
	g.finishUpload(gr, "a", 1, &ref)
	if g.beginUpload(gr, "a", 1, budget) == nil {
		t.Fatal("an output was accepted twice")
	}
	// All of one attempt's outputs share the budget (1 of 10 used).
	if err := g.beginUpload(gr, "big", 10, budget); err == nil {
		t.Fatal("an upload over the remaining budget was allowed")
	}
	if err := g.beginUpload(gr, "fits", 9, budget); err != nil {
		t.Fatalf("an upload within the budget was refused: %v", err)
	}
	if err := g.beginUpload(gr, "more", 1, budget); err == nil {
		t.Fatal("a concurrent upload past the reserved budget was allowed")
	}

	w := domain.Workload{ID: "w", Outputs: []string{"a", "b"}}
	got := g.verifiedOutputs(w, []domain.ArtifactRef{
		{Name: "b", SHA256: "bb", Size: 1}, // claimed, never uploaded
		{Name: "a", SHA256: "aa", Size: 1},
	})
	if len(got) != 1 || got[0] != ref {
		t.Fatalf("verifiedOutputs = %+v", got)
	}
	if got := g.verifiedOutputs(w, []domain.ArtifactRef{{Name: "a", SHA256: "zz", Size: 1}}); len(got) != 0 {
		t.Fatal("a claim with the wrong hash was accepted")
	}
}

func TestSettleOutputsDowngradesUndeliveredCompletion(t *testing.T) {
	s := &Server{grants: newGrantTable()}
	w := domain.Workload{ID: "w", Outputs: []string{"a"}}
	tok, _ := s.grants.mint("w", "n")

	running := domain.WorkloadStatus{State: domain.WorkloadRunning, Outputs: []domain.ArtifactRef{{Name: "a"}}}
	s.settleOutputs(w, &running)
	if running.Outputs != nil {
		t.Fatal("a non-terminal report may not claim outputs")
	}
	if _, ok := s.grants.lookup(tok); !ok {
		t.Fatal("a RUNNING report must keep the transfer grant")
	}

	done := domain.WorkloadStatus{State: domain.WorkloadCompleted, Outputs: []domain.ArtifactRef{{Name: "a", SHA256: "aa", Size: 1}}}
	s.settleOutputs(w, &done)
	if done.State != domain.WorkloadFailed || done.Outputs != nil {
		t.Fatalf("COMPLETED with an undelivered output: got %s %+v", done.State, done.Outputs)
	}
	if _, ok := s.grants.lookup(tok); ok {
		t.Fatal("a terminal report must drop the grant")
	}

	plain := domain.WorkloadStatus{State: domain.WorkloadCompleted, Outputs: []domain.ArtifactRef{{Name: "x"}}}
	s.settleOutputs(domain.Workload{ID: "p"}, &plain)
	if plain.State != domain.WorkloadCompleted || plain.Outputs != nil {
		t.Fatal("a workload without files can't gain outputs")
	}
}

// A grant outlives its assignment until it is swept or dropped; the
// request-time check must refuse it as soon as the workload is no longer
// PENDING/RUNNING on that node.
func TestGrantForRefusesFinishedOrMovedAssignments(t *testing.T) {
	store, err := artifacts.Open(artifacts.Config{Dir: t.TempDir(), MaxBytes: 1 << 10, TotalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{grants: newGrantTable(), Workloads: NewWorkloadRegistry(), cfg: Config{Artifacts: store}}
	const id = domain.WorkloadID("0123456789abcdef0123456789abcdef")
	s.Workloads.Put(domain.Workload{ID: id, Target: "n", Outputs: []string{"o"}}, domain.WorkloadStatus{ID: id, Target: "n", State: domain.WorkloadRunning})
	token, _ := s.grants.mint(id, "n")
	try := func() int {
		r := httptest.NewRequest("GET", "/workload-artifacts/"+string(id)+"/inputs/x", nil)
		r.SetPathValue("workload", string(id))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.grantFor(w, r)
		return w.Code
	}
	if code := try(); code != 200 {
		t.Fatalf("live assignment refused: %d", code)
	}
	s.Workloads.UpdateStatus(domain.WorkloadStatus{ID: id, Target: "n", State: domain.WorkloadFailed})
	if code := try(); code != 403 {
		t.Fatalf("finished assignment: got %d, want 403", code)
	}
	s.Workloads.Put(domain.Workload{ID: id, Target: "other", Outputs: []string{"o"}}, domain.WorkloadStatus{ID: id, Target: "other", State: domain.WorkloadRunning})
	if code := try(); code != 403 {
		t.Fatalf("assignment moved to another node: got %d, want 403", code)
	}
}
