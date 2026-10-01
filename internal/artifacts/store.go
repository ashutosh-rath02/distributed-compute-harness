// Package artifacts is the manager's content-addressed file store for
// workload inputs and outputs. Files are named by their SHA-256, so a
// stored artifact can't be silently changed and identical uploads are
// kept once. Every write is streamed through the hash and checked against
// a per-file and a whole-store size limit before it is committed.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"home-harness/internal/domain"
)

var (
	ErrTooLarge     = errors.New("artifacts: file exceeds the per-file size limit")
	ErrStoreFull    = errors.New("artifacts: artifact store is full")
	ErrHashMismatch = errors.New("artifacts: content does not match the expected sha256")
	ErrNotFound     = errors.New("artifacts: no such artifact (never stored, deleted, or expired)")
	ErrInvalidID    = errors.New("artifacts: an artifact ID must be 64 lowercase hex characters")
)

// Config sizes a Store.
type Config struct {
	Dir        string
	MaxBytes   int64 // largest single artifact
	TotalBytes int64 // whole store
}

// Info describes one stored artifact. ModTime is when it was stored or
// last used (Touch); garbage collection goes by it.
type Info struct {
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Store is safe for concurrent use.
type Store struct {
	cfg Config

	mu      sync.Mutex
	used    int64 // committed artifacts
	pending int64 // bytes written so far by uploads in progress
}

// Open opens (creating if needed) the store at cfg.Dir, discarding
// uploads a crash left half-written.
func Open(cfg Config) (*Store, error) {
	if cfg.MaxBytes <= 0 || cfg.TotalBytes <= 0 {
		return nil, fmt.Errorf("artifacts: MaxBytes and TotalBytes must be positive")
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "tmp"), filepath.Join(cfg.Dir, "sha256")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("artifacts: %w", err)
		}
	}
	if stale, err := os.ReadDir(filepath.Join(cfg.Dir, "tmp")); err == nil {
		for _, e := range stale {
			os.Remove(filepath.Join(cfg.Dir, "tmp", e.Name()))
		}
	}
	s := &Store{cfg: cfg}
	infos, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, in := range infos {
		s.used += in.Size
	}
	return s, nil
}

// MaxBytes is the per-artifact size limit.
func (s *Store) MaxBytes() int64 { return s.cfg.MaxBytes }

// Usage reports the bytes stored and the store's limit.
func (s *Store) Usage() (used, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used, s.cfg.TotalBytes
}

func (s *Store) path(sha string) string {
	return filepath.Join(s.cfg.Dir, "sha256", sha[:2], sha)
}

// Put stores r's content. If expectSHA is non-empty the content must hash
// to it. Writing stops as soon as the per-file or whole-store limit would
// be exceeded, and nothing partial is ever kept.
func (s *Store) Put(r io.Reader, expectSHA string) (Info, error) {
	return s.PutLimited(r, expectSHA, s.cfg.MaxBytes)
}

// PutLimited is Put with a tighter per-file limit (never above MaxBytes).
func (s *Store) PutLimited(r io.Reader, expectSHA string, limit int64) (Info, error) {
	if expectSHA != "" && !domain.ValidSHA256(expectSHA) {
		return Info{}, ErrInvalidID
	}
	if limit <= 0 || limit > s.cfg.MaxBytes {
		limit = s.cfg.MaxBytes
	}
	tmp, err := os.CreateTemp(filepath.Join(s.cfg.Dir, "tmp"), "upload-*")
	if err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	var written int64
	defer func() {
		s.mu.Lock()
		s.pending -= written
		s.mu.Unlock()
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if written+int64(n) > limit {
				return Info{}, ErrTooLarge
			}
			s.mu.Lock()
			full := s.used+s.pending+int64(n) > s.cfg.TotalBytes
			if !full {
				s.pending += int64(n)
			}
			s.mu.Unlock()
			if full {
				return Info{}, ErrStoreFull
			}
			written += int64(n)
			h.Write(buf[:n])
			if _, err := tmp.Write(buf[:n]); err != nil {
				return Info{}, fmt.Errorf("artifacts: %w", err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Info{}, rerr
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if expectSHA != "" && sum != expectSHA {
		return Info{}, ErrHashMismatch
	}
	if err := tmp.Sync(); err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}

	final := s.path(sum)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		// Already stored: keep the existing copy, mark it as used now.
		now := time.Now()
		os.Chtimes(final, now, now)
		os.Remove(tmpName)
		committed = true
		return Info{SHA256: sum, Size: written, ModTime: now}, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}
	committed = true
	s.used += written
	return Info{SHA256: sum, Size: written, ModTime: time.Now()}, nil
}

// Stat describes a stored artifact.
func (s *Store) Stat(sha string) (Info, error) {
	if !domain.ValidSHA256(sha) {
		return Info{}, ErrInvalidID
	}
	fi, err := os.Stat(s.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, fmt.Errorf("artifacts: %w", err)
	}
	return Info{SHA256: sha, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Open opens a stored artifact for reading.
func (s *Store) Open(sha string) (*os.File, Info, error) {
	info, err := s.Stat(sha)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(s.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Info{}, ErrNotFound
	}
	if err != nil {
		return nil, Info{}, fmt.Errorf("artifacts: %w", err)
	}
	return f, info, nil
}

// Touch marks an artifact as used now, so garbage collection keeps it for
// another retention period (done when work referencing it is submitted or
// dispatched).
func (s *Store) Touch(sha string) error {
	if !domain.ValidSHA256(sha) {
		return ErrInvalidID
	}
	now := time.Now()
	if err := os.Chtimes(s.path(sha), now, now); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	return nil
}

// Delete removes an artifact.
func (s *Store) Delete(sha string) error {
	info, err := s.Stat(sha)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(sha)); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	s.used -= info.Size
	return nil
}

// List returns every stored artifact, newest first.
func (s *Store) List() ([]Info, error) {
	var out []Info
	root := filepath.Join(s.cfg.Dir, "sha256")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !domain.ValidSHA256(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil // removed meanwhile
		}
		out = append(out, Info{SHA256: d.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// GC deletes artifacts last stored or used before cutoff that keep does
// not claim. A file that can't be removed right now (on Windows, one being
// downloaded) is left for the next run.
func (s *Store) GC(cutoff time.Time, keep func(sha string) bool) (removed int, freed int64) {
	infos, err := s.List()
	if err != nil {
		return 0, 0
	}
	for _, in := range infos {
		if !in.ModTime.Before(cutoff) || keep(in.SHA256) {
			continue
		}
		if s.Delete(in.SHA256) == nil {
			removed++
			freed += in.Size
		}
	}
	return removed, freed
}
