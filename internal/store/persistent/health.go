package persistent

import (
	"errors"
	"sync"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
)

// Health is whether the database can take writes right now. A full disk
// or a read-only filesystem does not corrupt a bbolt database (a commit
// that cannot finish is rolled back whole), so the manager keeps serving
// what it has in memory and on disk, refuses new work, and recovers on
// its own once writes succeed again.
type Health struct {
	// Degraded is true after a write failed for lack of space or because
	// the filesystem is read-only, until a later write succeeds.
	Degraded bool      `json:"degraded"`
	Reason   string    `json:"reason,omitempty"`
	Since    time.Time `json:"since,omitempty"`
}

type health struct {
	mu    sync.Mutex
	state Health
	fault error // test hook: returned instead of writing
	// epoch counts degradations. A write only clears the flag if it began
	// in the current epoch: one that was already in flight when the disk
	// filled may still succeed, and says nothing about writes now.
	epoch uint64
}

// healthDB is the bbolt database with every write (Update) watched.
// Embedding keeps View, Path, Close and the rest as they were.
type healthDB struct {
	*bbolt.DB
	h *health
}

func (d healthDB) Update(fn func(*bbolt.Tx) error) error {
	d.h.mu.Lock()
	fault, began := d.h.fault, d.h.epoch
	d.h.mu.Unlock()
	if fault != nil {
		d.h.observe(fault, began)
		return fault
	}
	// Batch, not Update: writers that arrive together share one commit and
	// one fsync (each still returns only after its data is durable). One
	// fsync per write capped the whole manager at the disk's sync rate
	// (see docs/operations.md); fns here are plain puts, safe to re-run if
	// bbolt retries a batch without a failing neighbour.
	err := d.DB.Batch(fn)
	d.h.observe(err, began)
	return err
}

func (h *health) observe(err error, began uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err == nil:
		if began == h.epoch {
			h.state = Health{}
		}
	case IsNoSpace(err):
		if !h.state.Degraded {
			h.epoch++
			h.state = Health{Degraded: true, Reason: ReasonFor(err), Since: time.Now().UTC()}
		}
	}
	// Any other error is the caller's own (a bad record, say): it says
	// nothing about the disk, so it changes nothing here.
}

// Health reports whether writes are currently failing for lack of space
// or a read-only filesystem.
func (s *Store) Health() Health {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.state
}

// Degraded is Health in a shape the manager can use without importing
// this package.
func (s *Store) Degraded() (bool, string, time.Time) {
	h := s.Health()
	return h.Degraded, h.Reason, h.Since
}

// Probe makes one tiny write, the cheapest honest test of whether the
// disk takes writes again. The manager calls it while degraded.
func (s *Store) Probe() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(settingsBucket).Put(probeKey, []byte(time.Now().UTC().Format(time.RFC3339)))
	})
}

var probeKey = []byte("health-probe")

// InjectWriteFault makes every write fail with err until it is called
// again with nil. It exists so tests (here and in test/integration) can
// stand in for a full or read-only disk, which cannot be produced
// portably without administrator rights.
func (s *Store) InjectWriteFault(err error) {
	s.h.mu.Lock()
	s.h.fault = err
	s.h.mu.Unlock()
}

// Sync flushes the database file to disk. Every commit already does; this
// is the last word before a shutdown.
func (s *Store) Sync() error { return s.db.Sync() }

// ErrNoSpace is what InjectWriteFault is usually given.
var ErrNoSpace = syscall.ENOSPC

// IsNoSpace reports whether err means the disk is full, over quota, or
// read-only, on any platform.
func IsNoSpace(err error) bool {
	var errno syscall.Errno
	return err != nil && errors.As(err, &errno) && noSpaceErrno(errno)
}

// ReasonFor is a plain-English cause for a storage error.
func ReasonFor(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) && readOnlyErrno(errno) {
		return "the disk is read-only"
	}
	return "the disk is full"
}
