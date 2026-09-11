package identity

import (
	"path/filepath"
	"testing"
)

func TestLoadOrCreateGeneratesOnFirstUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-a")
	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.NodeID == "" {
		t.Fatal("expected non-empty node id")
	}
	if len(id.PublicKey) == 0 {
		t.Fatal("expected non-empty public key")
	}
}

func TestLoadOrCreateIsStableAcrossReloads(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (first): %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (second): %v", err)
	}

	if first.NodeID != second.NodeID {
		t.Fatalf("expected stable node id across reloads, got %q then %q", first.NodeID, second.NodeID)
	}
}

func TestDifferentDirsProduceDifferentIdentities(t *testing.T) {
	a, err := LoadOrCreate(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatalf("LoadOrCreate (a): %v", err)
	}
	b, err := LoadOrCreate(filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("LoadOrCreate (b): %v", err)
	}
	if a.NodeID == b.NodeID {
		t.Fatal("expected distinct node ids for distinct identity dirs")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	msg := []byte("register-me")
	sig := id.Sign(msg)
	if !Verify(id.Identity, msg, sig) {
		t.Fatal("expected signature to verify against own identity")
	}
	if Verify(id.Identity, []byte("tampered"), sig) {
		t.Fatal("expected signature verification to fail for tampered message")
	}
}
