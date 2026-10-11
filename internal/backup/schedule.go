package backup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"home-harness/internal/fsperm"
)

const (
	filePrefix = "home-harness-backup-"
	extPlain   = ".zip"
	extSealed  = ".hhbk"
)

// WriteFile writes a backup to path atomically: into a hidden temp file in
// the same folder (owner-only, synced), then renamed over the target, so
// a crash or a full disk never leaves a half-written file under the real
// name. If seal is set the zip is encrypted on the way.
func WriteFile(path, passphrase string, iterations int, make func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := fsperm.Private(name); err != nil {
		return fail(err)
	}
	var w io.Writer = tmp
	var enc io.WriteCloser
	if passphrase != "" {
		if enc, err = NewEncryptWriter(tmp, passphrase, iterations); err != nil {
			return fail(err)
		}
		w = enc
	}
	if err := make(w); err != nil {
		return fail(err)
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Prune keeps the newest keep scheduled backups in dir and removes the
// rest (and leftovers of an interrupted write). It only ever touches files
// with this package's names. Returns the files removed.
func Prune(dir string, keep int) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed, kept []string
	for _, e := range ents {
		n := e.Name()
		switch {
		case strings.HasPrefix(n, ".partial-"):
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.Remove(filepath.Join(dir, n))
			}
		case strings.HasPrefix(n, filePrefix) && (strings.HasSuffix(n, extPlain) || strings.HasSuffix(n, extSealed)):
			kept = append(kept, n)
		}
	}
	sort.Strings(kept) // timestamped names sort oldest first
	if keep > 0 && len(kept) > keep {
		for _, n := range kept[:len(kept)-keep] {
			if os.Remove(filepath.Join(dir, n)) == nil {
				removed = append(removed, n)
			}
		}
	}
	return removed
}

// Last is the outcome of the most recent scheduled backup.
type Last struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Path  string    `json:"path,omitempty"`
	Bytes int64     `json:"bytes,omitempty"`
	Error string    `json:"error,omitempty"`
	Count int       `json:"kept,omitempty"`
}

// Schedule takes a backup every Every into Dir and keeps the newest Keep.
type Schedule struct {
	Dir        string
	Every      time.Duration
	Keep       int
	Passphrase string // empty: written unencrypted
	Iterations int    // 0: DefaultIterations
	Make       func(io.Writer) error

	mu   sync.Mutex
	last Last
}

// LastResult is the latest outcome (zero if none yet).
func (s *Schedule) LastResult() Last {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// RunOnce takes one backup now, checks the file it wrote (reads it back,
// verifies checksums, opens the database), then prunes old ones. A backup
// that fails its own check is deleted, and the old ones are kept.
func (s *Schedule) RunOnce() (path string, err error) {
	ext := extPlain
	if s.Passphrase != "" {
		ext = extSealed
	}
	path = filepath.Join(s.Dir, filePrefix+time.Now().UTC().Format("20060102T150405.000Z")+ext)
	defer func() {
		l := Last{At: time.Now().UTC(), OK: err == nil, Path: path}
		if err != nil {
			l.Error, l.Path = err.Error(), ""
		} else if st, serr := os.Stat(path); serr == nil {
			l.Bytes = st.Size()
		}
		s.mu.Lock()
		s.last = l
		s.mu.Unlock()
	}()
	if err = WriteFile(path, s.Passphrase, s.Iterations, s.Make); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	o, err := OpenFile(path, s.Passphrase)
	if err == nil {
		_, err = Verify(o)
		o.Close()
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("the new backup failed its own check and was removed: %w", err)
	}
	Prune(s.Dir, s.Keep)
	return path, nil
}

// Run takes a backup every Every until ctx ends, the first after one
// interval (a restart is not the moment to add load).
func (s *Schedule) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p, err := s.RunOnce(); err != nil {
				log.Printf("manager: scheduled backup failed: %v", err)
			} else {
				log.Printf("manager: backup written to %s", p)
			}
		}
	}
}
