package persistent

import (
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestSpotChecksAndSuspectsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A database without the buckets yet reads as empty.
	if got, err := s.ListSpotChecks(); err != nil || len(got) != 0 {
		t.Fatalf("fresh ListSpotChecks = %v, %v", got, err)
	}
	if got, err := s.ListSuspects(); err != nil || len(got) != 0 {
		t.Fatalf("fresh ListSuspects = %v, %v", got, err)
	}
	if err := s.DeleteSpotChecks([]domain.SpotCheck{{Job: "j", Task: "0000"}}); err != nil {
		t.Fatalf("delete before any write: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	a := domain.SpotCheck{Job: "j1", Task: "0000", Capability: "text.count", Original: "w1", Node: "n1", State: domain.SpotChecking, CreatedAt: now,
		Outputs: []domain.ArtifactRef{{Name: "counts.json", SHA256: "aa", Size: 2}}}
	b := domain.SpotCheck{Job: "j1", Task: "reduce", Original: "w2", Node: "n2", State: domain.SpotMatched, CreatedAt: now}
	for _, c := range []domain.SpotCheck{a, b} {
		if err := s.UpsertSpotCheck(c); err != nil {
			t.Fatal(err)
		}
	}
	a.State, a.Runs = domain.SpotMismatch, []domain.SpotCheckRun{{Workload: "w3", Node: "n3", State: domain.WorkloadCompleted}}
	if err := s.UpsertSpotCheck(a); err != nil { // same job and task: replaces
		t.Fatal(err)
	}
	if err := s.PutSuspect("n1", &domain.SuspectMark{Count: 2, Job: "j1", Task: "0000", Reason: "differed", Since: now, Last: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSuspect("n2", &domain.SuspectMark{Count: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSuspect("n2", nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checks, err := s.ListSpotChecks()
	if err != nil || len(checks) != 2 {
		t.Fatalf("ListSpotChecks = %v, %v", checks, err)
	}
	byTask := map[string]domain.SpotCheck{}
	for _, c := range checks {
		byTask[c.Task] = c
	}
	if got := byTask["0000"]; got.State != domain.SpotMismatch || len(got.Runs) != 1 || got.Outputs[0].SHA256 != "aa" || !got.CreatedAt.Equal(now) {
		t.Fatalf("reloaded check = %+v", got)
	}
	suspects, err := s.ListSuspects()
	if err != nil || len(suspects) != 1 || suspects["n1"].Count != 2 || suspects["n1"].Reason != "differed" {
		t.Fatalf("ListSuspects = %v, %v", suspects, err)
	}
	if err := s.DeleteSpotChecks([]domain.SpotCheck{a}); err != nil {
		t.Fatal(err)
	}
	if checks, _ := s.ListSpotChecks(); len(checks) != 1 || checks[0].Task != "reduce" {
		t.Fatalf("after delete = %v", checks)
	}
}
