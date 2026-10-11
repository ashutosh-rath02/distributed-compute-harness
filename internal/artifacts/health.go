package artifacts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"home-harness/internal/store/persistent"
)

// Health is whether the store can take files right now, and what garbage
// collection last did: the two things an operator wants to see when disk
// space is the question.
type Health struct {
	// NoSpace is true after a write failed for lack of space or on a
	// read-only disk, until a later write succeeds.
	NoSpace bool      `json:"noSpace"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitempty"`

	LastGC GCStats `json:"lastGc"`
}

// GCStats is the outcome of the latest garbage-collection pass.
type GCStats struct {
	At      time.Time `json:"at,omitempty"`
	Removed int       `json:"removed"`
	Freed   int64     `json:"freed"`
	// TotalRemoved and TotalFreed add up every pass since the manager started.
	TotalRemoved int   `json:"totalRemoved"`
	TotalFreed   int64 `json:"totalFreed"`
}

// IsNoSpace reports whether err means a full or read-only disk.
func IsNoSpace(err error) bool { return persistent.IsNoSpace(err) }

// ReasonFor is a plain-English cause for such an error.
func ReasonFor(err error) string { return persistent.ReasonFor(err) }

type healthState struct {
	mu    sync.Mutex
	h     Health
	fault error // test hook
}

func (s *Store) observe(err error) {
	s.hs.mu.Lock()
	defer s.hs.mu.Unlock()
	switch {
	case err == nil:
		s.hs.h.NoSpace, s.hs.h.Reason, s.hs.h.Since = false, "", time.Time{}
	case persistent.IsNoSpace(err) || errors.Is(err, syscall.ENOSPC):
		if !s.hs.h.NoSpace {
			s.hs.h.NoSpace, s.hs.h.Reason, s.hs.h.Since = true, persistent.ReasonFor(err), time.Now().UTC()
		}
	}
	// ErrTooLarge, a hash mismatch and the like say nothing about the disk.
}

func (s *Store) recordGC(removed int, freed int64) {
	s.hs.mu.Lock()
	defer s.hs.mu.Unlock()
	g := &s.hs.h.LastGC
	g.At, g.Removed, g.Freed = time.Now().UTC(), removed, freed
	g.TotalRemoved += removed
	g.TotalFreed += freed
}

// Health reports the store's write health and last garbage collection.
func (s *Store) Health() Health {
	s.hs.mu.Lock()
	defer s.hs.mu.Unlock()
	return s.hs.h
}

// InjectWriteFault makes Put fail with err until called with nil (a test
// stand-in for a full disk).
func (s *Store) InjectWriteFault(err error) {
	s.hs.mu.Lock()
	s.hs.fault = err
	s.hs.mu.Unlock()
}

func (s *Store) fault() error {
	s.hs.mu.Lock()
	defer s.hs.mu.Unlock()
	return s.hs.fault
}

// Probe writes and removes a small file: the cheapest honest test of
// whether the disk takes writes again.
func (s *Store) Probe() error {
	err := s.fault()
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(filepath.Join(s.cfg.Dir, "tmp"), "probe-*"); err == nil {
			_, err = io.WriteString(f, "ok")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			os.Remove(f.Name())
		}
	}
	s.observe(err)
	return err
}
