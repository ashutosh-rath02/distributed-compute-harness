package persistent

import (
	"os"
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
)

func TestFailoverStateAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if term, err := s.GetTerm(); term != 0 || err != nil {
		t.Fatalf("fresh term %d %v", term, err)
	}
	if _, found, _ := s.GetStandbyPair(); found {
		t.Fatal("fresh store has a pair")
	}
	v1, _ := s.Version()
	if err := s.PutTerm(3); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStandbyPair(domain.StandbyPair{Secret: "sec", PeerAddr: "10.0.0.2:7420"}); err != nil {
		t.Fatal(err)
	}
	v2, _ := s.Version()
	if v2 == v1 {
		t.Fatal("Version didn't change with a write")
	}

	// A snapshot is a complete database with the same state.
	snap := filepath.Join(dir, "copy.db")
	f, _ := os.Create(snap)
	txid, err := s.Snapshot(f)
	f.Close()
	if err != nil || txid != v2 {
		t.Fatalf("snapshot txid %d (version %d): %v", txid, v2, err)
	}
	c, err := Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	term, _ := c.GetTerm()
	pair, found, _ := c.GetStandbyPair()
	c.Close()
	if term != 3 || !found || pair.Secret != "sec" {
		t.Fatalf("copy: term %d, pair %+v", term, pair)
	}

	if err := s.DeleteStandbyPair(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetStandbyPair(); found {
		t.Fatal("pair survived delete")
	}
	if s.Path() != filepath.Join(dir, "manager.db") {
		t.Fatalf("Path %s", s.Path())
	}
}
