package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The first certificate seen is pinned at once — including for the
// retries while the device waits for approval — and kept on disk only
// once the manager has admitted the device.
func TestFirstUsePinsTheFirstCertificate(t *testing.T) {
	file := filepath.Join(t.TempDir(), pinnedFingerprintFile)
	f := &firstUse{file: file}
	verify := f.config().VerifyPeerCertificate
	real, other := []byte("the real manager's certificate"), []byte("someone else's certificate")

	if err := verify([][]byte{real}, nil); err != nil {
		t.Fatalf("first certificate: %v", err)
	}
	sum := sha256.Sum256(real)
	if f.fingerprint() != hex.EncodeToString(sum[:]) {
		t.Fatalf("pinned %q", f.fingerprint())
	}
	if err := verify([][]byte{other}, nil); err == nil {
		t.Fatal("a different certificate on a later connection must be refused")
	}
	if err := verify([][]byte{real}, nil); err != nil {
		t.Fatalf("the pinned certificate again: %v", err)
	}
	if err := verify(nil, nil); err == nil {
		t.Fatal("no certificate at all must be refused")
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("nothing may be written before the manager admits the device")
	}

	f.keep()
	data, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(data)) != f.fingerprint() {
		t.Fatalf("kept %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
			t.Fatalf("the pin file must be owner-only, got %v", info.Mode().Perm())
		}
	}
}
