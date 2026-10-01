package artifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func openStore(t *testing.T, dir string, maxBytes, total int64) *Store {
	t.Helper()
	s, err := Open(Config{Dir: dir, MaxBytes: maxBytes, TotalBytes: total})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func tmpFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestPutStoresContentAddressedAndDedupes(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, 1<<20, 1<<20)
	data := []byte("hello artifacts")
	info, err := s.Put(bytes.NewReader(data), sum(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.SHA256 != sum(data) || info.Size != int64(len(data)) {
		t.Fatalf("Put returned %+v", info)
	}
	if _, err := s.Put(bytes.NewReader(data), ""); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if used, _ := s.Usage(); used != int64(len(data)) {
		t.Fatalf("a duplicate upload must be stored once, usage = %d", used)
	}
	f, got, err := s.Open(info.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	content, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(content, data) || got.Size != int64(len(data)) {
		t.Fatalf("read back %q (%+v)", content, got)
	}
	if tmpFiles(t, dir) != 0 {
		t.Fatal("temp files left behind")
	}

	// Usage survives reopening.
	s2 := openStore(t, dir, 1<<20, 1<<20)
	if used, _ := s2.Usage(); used != int64(len(data)) {
		t.Fatalf("usage after reopen = %d", used)
	}
}

func TestPutRejectsMismatchOversizeAndFullStoreLeavingNothing(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, 100, 150)
	data := []byte("some content")
	if _, err := s.Put(bytes.NewReader(data), strings.Repeat("0", 64)); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("wrong expected hash: got %v", err)
	}
	if _, err := s.Put(bytes.NewReader(data), "NOT-HEX"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid expected hash: got %v", err)
	}
	if _, err := s.Put(bytes.NewReader(make([]byte, 101)), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over per-file limit: got %v", err)
	}
	if _, err := s.PutLimited(bytes.NewReader(make([]byte, 20)), "", 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over a tighter limit: got %v", err)
	}
	if _, err := s.Put(bytes.NewReader(bytes.Repeat([]byte("a"), 90)), ""); err != nil {
		t.Fatalf("first 90 bytes: %v", err)
	}
	if _, err := s.Put(bytes.NewReader(bytes.Repeat([]byte("b"), 90)), ""); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("90 more into a 150-byte store: got %v", err)
	}
	if used, _ := s.Usage(); used != 90 {
		t.Fatalf("failed uploads must not count, usage = %d", used)
	}
	if tmpFiles(t, dir) != 0 {
		t.Fatal("a failed upload left a temp file")
	}
	if list, _ := s.List(); len(list) != 1 {
		t.Fatalf("expected exactly the one good artifact stored, got %d", len(list))
	}
}

func TestInvalidIDsNeverTouchThePath(t *testing.T) {
	s := openStore(t, t.TempDir(), 100, 100)
	for _, id := range []string{"", "..", "../../etc/passwd", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, err := s.Stat(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Stat(%q) = %v", id, err)
		}
		if _, _, err := s.Open(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Open(%q) = %v", id, err)
		}
		if err := s.Delete(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Delete(%q) = %v", id, err)
		}
	}
	if _, err := s.Stat(strings.Repeat("a", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown artifact: got %v", err)
	}
}

func TestGCRemovesOnlyOldUnclaimedArtifacts(t *testing.T) {
	s := openStore(t, t.TempDir(), 1<<20, 1<<20)
	put := func(c string) string {
		info, err := s.Put(strings.NewReader(c), "")
		if err != nil {
			t.Fatal(err)
		}
		return info.SHA256
	}
	old, claimed, fresh := put("old"), put("claimed"), put("fresh")
	past := time.Now().Add(-48 * time.Hour)
	for _, sha := range []string{old, claimed} {
		os.Chtimes(s.path(sha), past, past)
	}
	removed, freed := s.GC(time.Now().Add(-24*time.Hour), func(sha string) bool { return sha == claimed })
	if removed != 1 || freed != 3 {
		t.Fatalf("GC removed %d (%d bytes), want 1 (3 bytes)", removed, freed)
	}
	if _, err := s.Stat(old); !errors.Is(err, ErrNotFound) {
		t.Fatal("old unclaimed artifact survived GC")
	}
	for _, sha := range []string{claimed, fresh} {
		if _, err := s.Stat(sha); err != nil {
			t.Fatalf("GC removed a claimed or fresh artifact: %v", err)
		}
	}
	// Touch protects an old artifact for another period.
	os.Chtimes(s.path(claimed), past, past)
	if err := s.Touch(claimed); err != nil {
		t.Fatal(err)
	}
	if removed, _ := s.GC(time.Now().Add(-24*time.Hour), func(string) bool { return false }); removed != 0 {
		t.Fatal("a just-touched artifact was collected")
	}
	if used, _ := s.Usage(); used != int64(len("claimed")+len("fresh")) {
		t.Fatalf("usage after GC = %d", used)
	}
}
