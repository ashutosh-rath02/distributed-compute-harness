package artifacts

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// A clock set back leaves files looking newer than now: GC must keep them.
func TestGCKeepsFilesFromAFutureClock(t *testing.T) {
	s := openStore(t, t.TempDir(), 1<<20, 8<<20)
	info, err := s.Put(bytes.NewReader([]byte("data")), "")
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(48 * time.Hour)
	if err := os.Chtimes(s.path(info.SHA256), future, future); err != nil {
		t.Fatal(err)
	}
	removed, _ := s.GC(time.Now().Add(-time.Hour), func(string) bool { return false })
	if removed != 0 {
		t.Fatal("GC removed a file whose timestamp is in the future")
	}
	if g := s.Health().LastGC; g.At.IsZero() || g.Removed != 0 {
		t.Errorf("GC pass not recorded: %+v", g)
	}
}

func TestFullDiskIsReportedAndClearsOnTheNextGoodWrite(t *testing.T) {
	s := openStore(t, t.TempDir(), 1<<20, 8<<20)
	s.InjectWriteFault(syscall.ENOSPC)
	if _, err := s.Put(bytes.NewReader([]byte("x")), ""); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Put under a full disk: %v", err)
	}
	if h := s.Health(); !h.NoSpace || h.Reason == "" {
		t.Fatalf("health = %+v", h)
	}
	if s.Probe() == nil {
		t.Fatal("probe should fail while full")
	}
	s.InjectWriteFault(nil)
	if err := s.Probe(); err != nil {
		t.Fatal(err)
	}
	if s.Health().NoSpace {
		t.Fatal("still flagged after a good write")
	}
}
