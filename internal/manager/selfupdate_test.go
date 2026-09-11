package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
)

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.exe")
	content := []byte("agent binary content")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hashFile = %q, want %q", got, want)
	}
}

func newTestServerWithAgentBinaryHash(hash string) *Server {
	return &Server{agentBinaryHash: hash}
}

func TestNeedsUpdateDisabledWhenNoAgentBinaryConfigured(t *testing.T) {
	s := newTestServerWithAgentBinaryHash("") // -agent-binary unset
	rec := &NodeRecord{Node: domain.Node{BinaryHash: "some-hash"}}
	if s.NeedsUpdate(rec) {
		t.Fatal("expected NeedsUpdate false when self-update is disabled")
	}
}

func TestNeedsUpdateFalseWhenNodeNeverReportedAHash(t *testing.T) {
	s := newTestServerWithAgentBinaryHash("current-hash")
	rec := &NodeRecord{Node: domain.Node{BinaryHash: ""}} // pre-v4-part-2 agent
	if s.NeedsUpdate(rec) {
		t.Fatal("expected NeedsUpdate false for a node that never reported a BinaryHash")
	}
}

func TestNeedsUpdateFalseWhenHashesMatch(t *testing.T) {
	s := newTestServerWithAgentBinaryHash("current-hash")
	rec := &NodeRecord{Node: domain.Node{BinaryHash: "current-hash"}}
	if s.NeedsUpdate(rec) {
		t.Fatal("expected NeedsUpdate false when the node already matches the served binary")
	}
}

func TestNeedsUpdateTrueWhenHashesDiffer(t *testing.T) {
	s := newTestServerWithAgentBinaryHash("current-hash")
	rec := &NodeRecord{Node: domain.Node{BinaryHash: "old-hash"}}
	if !s.NeedsUpdate(rec) {
		t.Fatal("expected NeedsUpdate true when the node's reported hash differs")
	}
}
