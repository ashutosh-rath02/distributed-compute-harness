package persistent

import (
	"path/filepath"
	"testing"
	"time"

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

func TestRevokeNodeForgetsRecordAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.UpsertNode(testManifest("node-a", "A"))
	s.UpsertNode(testManifest("node-b", "B"))

	rev := domain.RevokedNode{NodeID: "node-a", Name: "A", RevokedAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.RevokeNode(rev); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if _, found, _ := s.GetNode("node-a"); found {
		t.Fatal("expected a revoked node's record to be forgotten in the same transaction")
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	revoked, err := s.ListRevoked()
	if err != nil {
		t.Fatalf("ListRevoked: %v", err)
	}
	if len(revoked) != 1 || revoked[0].NodeID != "node-a" || revoked[0].Name != "A" || !revoked[0].RevokedAt.Equal(rev.RevokedAt) {
		t.Fatalf("expected node-a's revocation to survive reopen, got %+v", revoked)
	}
	if nodes, _ := s.ListNodes(); len(nodes) != 1 || nodes[0].Node.Identity.NodeID != "node-b" {
		t.Fatalf("expected only node-b to remain known, got %+v", nodes)
	}

	if err := s.UnrevokeNode("node-a"); err != nil {
		t.Fatalf("UnrevokeNode: %v", err)
	}
	if revoked, _ := s.ListRevoked(); len(revoked) != 0 {
		t.Fatalf("expected no revocations after UnrevokeNode, got %+v", revoked)
	}
}

func TestNodeMetaPersistsAndIsClearedOnRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.UpsertNode(testManifest("node-a", "A"))
	meta := domain.NodeMeta{Alias: "Living-room PC", Labels: map[string]string{"gpu": "iris-xe"}}
	if err := s.PutNodeMeta("node-a", meta); err != nil {
		t.Fatalf("PutNodeMeta: %v", err)
	}
	s.PutNodeMeta("node-b", domain.NodeMeta{Alias: "B"})
	s.PutNodeMeta("node-b", domain.NodeMeta{}) // empty deletes
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	all, err := s.ListNodeMeta()
	if err != nil || len(all) != 1 || all["node-a"].Alias != "Living-room PC" || all["node-a"].Labels["gpu"] != "iris-xe" {
		t.Fatalf("expected node-a's metadata to survive reopen (and node-b's to be deleted), got %+v %v", all, err)
	}
	if err := s.RevokeNode(domain.RevokedNode{NodeID: "node-a"}); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if all, _ := s.ListNodeMeta(); len(all) != 0 {
		t.Fatalf("expected revocation to clear the node's metadata, got %+v", all)
	}
}

func TestAuditLogsAreOrderedTrimmedAndIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.AppendAudit(domain.AuditEntry{Log: domain.AuditSecurity, Kind: "node.revoked", NodeID: "node-a"}, 5); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	// Flood the noise log far past its cap: it must trim itself without
	// touching the security log.
	for i := 0; i < 50; i++ {
		if _, err := s.AppendAudit(domain.AuditEntry{Log: domain.AuditNoise, Kind: "node.rejected"}, 10); err != nil {
			t.Fatalf("AppendAudit noise: %v", err)
		}
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	noise, _ := s.ListAudit(domain.AuditNoise, 0)
	if len(noise) != 10 || noise[0].Seq != 50 || noise[9].Seq != 41 {
		t.Fatalf("expected the newest 10 noise entries, newest first, got %d (first seq %d)", len(noise), noise[0].Seq)
	}
	security, _ := s.ListAudit(domain.AuditSecurity, 0)
	if len(security) != 1 || security[0].Kind != "node.revoked" {
		t.Fatalf("expected the security log untouched by noise, got %+v", security)
	}
	if limited, _ := s.ListAudit(domain.AuditNoise, 3); len(limited) != 3 || limited[0].Seq != 50 {
		t.Fatalf("expected limit to return the newest 3, got %+v", limited)
	}
	if _, err := s.AppendAudit(domain.AuditEntry{Log: "bogus"}, 1); err == nil {
		t.Fatal("expected an unknown audit log to be refused")
	}
}
