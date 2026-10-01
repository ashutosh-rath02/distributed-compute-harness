// Package instancelock ensures a single running agent process per node
// identity. Every onboarding launcher restarts the agent whenever it
// exits, and a self-update relaunches the new binary itself — so without
// a lock, one identity could end up with two live processes (each taking
// its own workload slots, fighting over the manager connection). The lock
// is an OS file lock held for the process's lifetime: the kernel releases
// it on exit, even a crash, so a stale lock can never block a restart.
package instancelock

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrHeld means another live process holds the lock.
var ErrHeld = errors.New("instancelock: held by another process")

// Lock is a held instance lock.
type Lock struct{ f *os.File }

// TryAcquire takes the lock file in dir without blocking, returning
// ErrHeld if another process holds it.
func TryAcquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock (also released automatically at process exit).
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockFile(l.f)
	return l.f.Close()
}
