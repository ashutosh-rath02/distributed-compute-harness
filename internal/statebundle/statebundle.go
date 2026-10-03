// Package statebundle moves a manager's whole state directory between
// installs — e.g. from the Termux manager to the Android app (roadmap
// item 9) — as one zip: the pairing and operator tokens, the database and
// the TLS certificate every agent has pinned, so the fleet reconnects to
// the new install unchanged. The bundle holds the TLS private key and
// both tokens: treat it like a password and delete it after importing.
package statebundle

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// DBName is the manager database inside a state directory; it is
// exported as a consistent snapshot, and its presence is what makes a
// state directory "in use".
const DBName = "manager.db"

// Files never worth moving.
var skipped = map[string]bool{"manager.pid": true, "manager.log": true, "manager.log.1": true}

const maxFileBytes = 1 << 30

// ErrRunning means a manager holds the state's database open.
var ErrRunning = errors.New("statebundle: a manager is using this state directory (stop it first)")

// ErrExists means import would overwrite an existing state.
var ErrExists = errors.New("statebundle: the state directory already has a database (pass force to replace it)")

// Export writes stateDir as a zip. The database is copied from a
// read-only transaction (a consistent snapshot); that open fails if a
// manager is running, since it holds the database's exclusive lock.
// artifacts/ is included only with withArtifacts.
func Export(stateDir string, w io.Writer, withArtifacts bool) error {
	zw := zip.NewWriter(w)
	dbPath := filepath.Join(stateDir, DBName)
	if _, err := os.Stat(dbPath); err == nil {
		db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
		if errors.Is(err, bbolt.ErrTimeout) {
			return ErrRunning
		}
		if err != nil {
			return fmt.Errorf("statebundle: open database: %w", err)
		}
		err = db.View(func(tx *bbolt.Tx) error {
			out, err := zw.Create(DBName)
			if err != nil {
				return err
			}
			_, err = tx.WriteTo(out)
			return err
		})
		db.Close()
		if err != nil {
			return fmt.Errorf("statebundle: snapshot database: %w", err)
		}
	}
	err := filepath.WalkDir(stateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stateDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "artifacts" && !withArtifacts {
				return fs.SkipDir
			}
			return nil
		}
		if rel == DBName || skipped[rel] || strings.HasSuffix(rel, ".lock") || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Method = rel, zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(out, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("statebundle: %w", err)
	}
	return zw.Close()
}

// Import unpacks a bundle into stateDir. Every entry must be a plain
// relative path inside it; an existing database is only replaced with
// force. Files are written owner-only (they include the TLS key and the
// tokens).
func Import(r io.ReaderAt, size int64, stateDir string, force bool) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("statebundle: not a state bundle: %w", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, DBName)); err == nil && !force {
		return ErrExists
	}
	hasDB := false
	for _, f := range zr.File {
		if err := checkName(f.Name); err != nil {
			return err
		}
		hasDB = hasDB || f.Name == DBName
	}
	if !hasDB {
		return errors.New("statebundle: the bundle has no manager.db — not a manager state export")
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if f.UncompressedSize64 > maxFileBytes {
			return fmt.Errorf("statebundle: %s is too large", f.Name)
		}
		dest := filepath.Join(stateDir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		tmp := dest + ".importing"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(in, maxFileBytes))
		in.Close()
		closeErr := out.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			os.Remove(tmp)
			return fmt.Errorf("statebundle: %s: %w", f.Name, copyErr)
		}
		os.Remove(dest) // Windows rename doesn't replace
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
	}
	return nil
}

// checkName accepts only a relative, slash-separated path that stays
// inside the state directory.
func checkName(name string) error {
	clean := path.Clean(name)
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || path.IsAbs(name) ||
		clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(name, ":") || clean != strings.TrimSuffix(name, "/") {
		return fmt.Errorf("statebundle: refusing entry %q (not a plain relative path)", name)
	}
	return nil
}
