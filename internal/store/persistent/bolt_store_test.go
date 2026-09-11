package persistent

import (
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testManifest(id domain.NodeID, name string) domain.Manifest {
	return domain.Manifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Node:          domain.Node{Identity: domain.Identity{NodeID: id}, Name: name},
	}
}

func TestUpsertAndGetNode(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertNode(testManifest("node-a", "Laptop-A")); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	rec, found, err := s.GetNode("node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !found {
		t.Fatal("expected node-a to be found")
	}
	if rec.Manifest.Node.Name != "Laptop-A" {
		t.Fatalf("expected name %q, got %q", "Laptop-A", rec.Manifest.Node.Name)
	}
	if rec.FirstRegisteredAt.IsZero() || rec.LastRegisteredAt.IsZero() {
		t.Fatal("expected non-zero registration timestamps")
	}
}

func TestGetNodeUnknownReturnsNotFound(t *testing.T) {
	s := openTestStore(t)
	_, found, err := s.GetNode("node-does-not-exist")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if found {
		t.Fatal("expected unknown node to not be found")
	}
}

func TestUpsertPreservesFirstRegisteredAt(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertNode(testManifest("node-a", "Laptop-A")); err != nil {
		t.Fatalf("UpsertNode (1): %v", err)
	}
	first, _, _ := s.GetNode("node-a")

	if err := s.UpsertNode(testManifest("node-a", "Laptop-A-Renamed")); err != nil {
		t.Fatalf("UpsertNode (2): %v", err)
	}
	second, _, _ := s.GetNode("node-a")

	if !second.FirstRegisteredAt.Equal(first.FirstRegisteredAt) {
		t.Fatalf("expected FirstRegisteredAt to stay stable across re-registration, got %v then %v", first.FirstRegisteredAt, second.FirstRegisteredAt)
	}
	if second.Manifest.Node.Name != "Laptop-A-Renamed" {
		t.Fatalf("expected updated name to be persisted, got %q", second.Manifest.Node.Name)
	}
}

func TestListNodesReturnsAllRecords(t *testing.T) {
	s := openTestStore(t)
	s.UpsertNode(testManifest("node-a", "A"))
	s.UpsertNode(testManifest("node-b", "B"))

	records, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
}

func TestDeleteNode(t *testing.T) {
	s := openTestStore(t)
	s.UpsertNode(testManifest("node-a", "A"))

	if err := s.DeleteNode("node-a"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	_, found, err := s.GetNode("node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if found {
		t.Fatal("expected node-a to be gone after DeleteNode")
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (1): %v", err)
	}
	if err := s1.UpsertNode(testManifest("node-a", "Laptop-A")); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (2): %v", err)
	}
	defer s2.Close()

	rec, found, err := s2.GetNode("node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !found {
		t.Fatal("expected node-a to survive close+reopen")
	}
	if rec.Manifest.Node.Name != "Laptop-A" {
		t.Fatalf("expected name %q, got %q", "Laptop-A", rec.Manifest.Node.Name)
	}
}
