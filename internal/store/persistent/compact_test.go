package persistent

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
)

func TestCompactShrinksAndKeepsEveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		s.UpsertJob(domain.Job{ID: domain.JobID(fmt.Sprintf("job-%04d", i)), Name: "padding padding padding padding padding padding padding padding"})
	}
	// Delete most of them: bbolt keeps the pages, the file does not shrink.
	var ids []domain.WorkloadID
	for i := 0; i < 600; i++ {
		ids = append(ids, domain.WorkloadID(fmt.Sprintf("w-%04d", i)))
		pw := domain.PersistedWorkload{}
		pw.Workload.ID = ids[i]
		s.UpsertWorkload(pw)
	}
	s.DeleteWorkloads(ids)
	s.Close()

	before, after, err := Compact(path)
	if err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Errorf("compaction did not shrink the file: %d -> %d", before, after)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if jobs, _ := s2.ListJobs(); len(jobs) != 600 {
		t.Errorf("%d jobs after compaction, want 600", len(jobs))
	}
	if v, _ := s2.SchemaVersion(); v != CurrentSchema {
		t.Errorf("schema version %d after compaction", v)
	}
}

func TestCompactRefusesALiveDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := Compact(path); err == nil {
		t.Fatal("compacted a database a manager has open")
	}
}

// A write that began before the disk filled and finished after must not
// make the store look healthy again.
func TestSuccessOfAnEarlierWriteDoesNotClearDegraded(t *testing.T) {
	h := &health{}
	began := h.epoch
	h.observe(ErrNoSpace, h.epoch) // the disk fills
	h.observe(nil, began)          // an older write lands
	if !h.state.Degraded {
		t.Fatal("an older write's success cleared the degraded flag")
	}
	h.observe(nil, h.epoch) // a write that began after the fill
	if h.state.Degraded {
		t.Fatal("a write after the fill should clear it")
	}
	_ = errors.New
}
