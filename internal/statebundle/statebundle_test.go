package statebundle

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func writeState(t *testing.T, dir string) {
	t.Helper()
	for name, content := range map[string]string{
		"pairing-token": "pair", "operator-token": "op", "ai-key": "ai", "tls/cert.pem": "CERT", "tls/key.pem": "KEY",
		"artifacts/sha256/ab/abc": "blob", "manager.pid": "123", "manager.log": "noise",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(content), 0o600)
	}
	db, err := bbolt.Open(filepath.Join(dir, DBName), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bbolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists([]byte("nodes"))
		return b.Put([]byte("node-1"), []byte("manifest"))
	})
	db.Close()
}

func TestExportImportRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeState(t, src)
	var buf bytes.Buffer
	if err := Export(src, &buf, false); err != nil {
		t.Fatal(err)
	}
	if err := Import(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dst, false); err != nil {
		t.Fatal(err)
	}
	// The AI key moves too, so apps set up with it keep working.
	for name, want := range map[string]string{"pairing-token": "pair", "operator-token": "op", "ai-key": "ai", "tls/cert.pem": "CERT", "tls/key.pem": "KEY"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	for _, name := range []string{"manager.pid", "manager.log", "artifacts/sha256/ab/abc"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("%s should not move", name)
		}
	}
	db, err := bbolt.Open(filepath.Join(dst, DBName), 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket([]byte("nodes")).Get([]byte("node-1")); string(v) != "manifest" {
			t.Errorf("database content lost: %q", v)
		}
		return nil
	})
	// An existing state needs force.
	if err := Import(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dst, false); !errors.Is(err, ErrExists) {
		t.Fatalf("second import without force: %v", err)
	}
	var withArt bytes.Buffer
	if err := Export(src, &withArt, true); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(withArt.Bytes()), int64(withArt.Len()))
	found := false
	for _, f := range zr.File {
		found = found || f.Name == "artifacts/sha256/ab/abc"
	}
	if !found {
		t.Fatal("-with-artifacts didn't include the artifacts")
	}
}

func TestExportRefusesARunningManager(t *testing.T) {
	src := t.TempDir()
	writeState(t, src)
	db, err := bbolt.Open(filepath.Join(src, DBName), 0o600, nil) // the "running manager"
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Export(src, &bytes.Buffer{}, false); !errors.Is(err, ErrRunning) {
		t.Fatalf("export with the database open: %v", err)
	}
}

func TestImportRefusesEscapingNames(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/etc/x", `..\evil`, `C:\x`, "c:x", "tls/../../x"} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(DBName)
		w.Write([]byte("x"))
		w, _ = zw.Create(name)
		w.Write([]byte("x"))
		zw.Close()
		dst := t.TempDir()
		if err := Import(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dst, false); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%q: %v", name, err)
		}
		if entries, _ := os.ReadDir(dst); len(entries) != 0 {
			t.Errorf("%q: something was written before refusing", name)
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("pairing-token")
	w.Write([]byte("x"))
	zw.Close()
	if err := Import(bytes.NewReader(buf.Bytes()), int64(buf.Len()), t.TempDir(), false); err == nil {
		t.Fatal("a bundle without a database was accepted")
	}
}
