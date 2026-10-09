package backup

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/fsperm"
)

// ErrRunning means a manager has the database open.
var ErrRunning = errors.New("backup: a manager is running on this database (stop it first)")

// ErrExists means the target already has a database.
var ErrExists = errors.New("backup: the target already has a database (pass force to replace it; the old files are kept as .pre-restore)")

// CheckNotRunning fails with ErrRunning if a live manager holds the
// database: bbolt gives the process that opened it an exclusive file lock,
// so a second open times out for as long as the manager is up.
func CheckNotRunning(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 1500 * time.Millisecond})
	if errors.Is(err, bbolt.ErrTimeout) {
		return ErrRunning
	}
	if err != nil {
		return nil // damaged or unreadable: restoring over it is the point
	}
	return db.Close()
}

func (l Layout) destFor(name string) (string, bool) {
	switch name {
	case entryDB:
		return l.DB, l.DB != ""
	case entryOperator:
		return l.OperatorToken, l.OperatorToken != ""
	case entryAIKey:
		return l.AIKey, l.AIKey != ""
	case entryRemote:
		return l.RemoteAccess, l.RemoteAccess != ""
	case entryPairing:
		return l.PairingToken, l.PairingToken != ""
	}
	if rest, ok := strings.CutPrefix(name, dirTLS); ok && l.TLSDir != "" {
		return filepath.Join(l.TLSDir, rest), true
	}
	if rest, ok := strings.CutPrefix(name, dirArtifacts); ok && l.ArtifactDir != "" {
		return filepath.Join(l.ArtifactDir, filepath.FromSlash(rest)), true
	}
	return "", false
}

// Restore verifies the backup, then puts it in place: every file is first
// written next to its destination, then moved over it, the database last.
// Anything replaced is kept as <name>.pre-restore. It refuses to run over
// a live manager, and over an existing database unless force. Skipped
// lists entries the layout has nowhere to put (e.g. a pairing token when
// the manager takes it as a flag).
func Restore(o *Open, l Layout, force bool) (rep *Report, skipped []string, err error) {
	if rep, err = Verify(o); err != nil {
		return nil, nil, err
	}
	if err := CheckNotRunning(l.DB); err != nil {
		return rep, nil, err
	}
	if _, err := os.Stat(l.DB); err == nil && !force {
		return rep, nil, ErrExists
	}
	zr, _ := zip.NewReader(o.F, o.Size)
	type move struct{ staged, dest string }
	var moves []move
	var dbMove *move
	cleanup := func() {
		for _, m := range moves {
			os.Remove(m.staged)
		}
		if dbMove != nil {
			os.Remove(dbMove.staged)
		}
	}
	for _, f := range zr.File {
		if f.Name == entryManifest {
			continue
		}
		dest, ok := l.destFor(f.Name)
		if !ok {
			skipped = append(skipped, f.Name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			cleanup()
			return rep, nil, err
		}
		staged := dest + ".restoring"
		if err := extract(f, staged); err != nil {
			os.Remove(staged)
			cleanup()
			return rep, nil, fmt.Errorf("backup: restore %s: %w", f.Name, err)
		}
		if f.Name == entryDB {
			dbMove = &move{staged, dest}
		} else {
			moves = append(moves, move{staged, dest})
		}
	}
	if dbMove != nil {
		moves = append(moves, *dbMove)
		dbMove = nil
	}
	for i, m := range moves {
		if _, err := os.Stat(m.dest); err == nil {
			os.Remove(m.dest + ".pre-restore")
			if err := os.Rename(m.dest, m.dest+".pre-restore"); err != nil {
				moves = moves[i:]
				cleanup()
				return rep, skipped, fmt.Errorf("backup: set aside %s: %w", m.dest, err)
			}
		}
		if err := os.Rename(m.staged, m.dest); err != nil {
			moves = moves[i:]
			cleanup()
			return rep, skipped, fmt.Errorf("backup: put %s in place: %w", m.dest, err)
		}
	}
	return rep, skipped, nil
}

// extract writes one entry to dest, owner-only, synced.
func extract(f *zip.File, dest string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, int64(f.UncompressedSize64)+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsperm.Private(dest)
	}
	return err
}
