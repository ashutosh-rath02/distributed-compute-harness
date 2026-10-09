package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"home-harness/internal/domain"
	"home-harness/internal/store/persistent"
)

const testIter = 1000

// makeState builds a state directory with a real database, identity and
// tokens, and returns its layout and a Source over it.
func makeState(t *testing.T, withArtifact bool) (Layout, Source, *persistent.Store) {
	t.Helper()
	dir := t.TempDir()
	l := StateDirLayout(dir)
	st, err := persistent.Open(l.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 0; i < 20; i++ {
		if err := st.UpsertJob(domain.Job{ID: domain.JobID("job-" + string(rune('a'+i)))}); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(l.TLSDir, 0o700)
	os.WriteFile(filepath.Join(l.TLSDir, "manager-key.pem"), []byte("KEY"), 0o600)
	os.WriteFile(filepath.Join(l.TLSDir, "manager-cert.pem"), []byte("CERT"), 0o644)
	os.WriteFile(l.OperatorToken, []byte("optoken"), 0o600)
	os.WriteFile(l.AIKey, []byte("aikey"), 0o600)
	if withArtifact {
		sha := strings.Repeat("ab", 32)
		os.MkdirAll(filepath.Join(l.ArtifactDir, "sha256", "ab"), 0o700)
		os.WriteFile(filepath.Join(l.ArtifactDir, "sha256", "ab", sha), []byte("file body"), 0o600)
		os.MkdirAll(filepath.Join(l.ArtifactDir, "tmp"), 0o700)
		os.WriteFile(filepath.Join(l.ArtifactDir, "tmp", "upload-1"), []byte("half"), 0o600)
	}
	return l, Source{Layout: l, SnapshotDB: st.Snapshot, PairingToken: "pair-by-value", IncludeArtifacts: withArtifact, TempDir: t.TempDir()}, st
}

func writeBackup(t *testing.T, src Source, pass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.bak")
	err := WriteFile(p, pass, testIter, func(w io.Writer) error { _, err := Create(w, src); return err })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoundTripPlainAndEncrypted(t *testing.T) {
	for _, pass := range []string{"", "correct horse"} {
		l, src, st := makeState(t, true)
		p := writeBackup(t, src, pass)
		raw, _ := os.ReadFile(p)
		if (pass != "") != IsEncrypted(raw) {
			t.Fatalf("pass=%q: encrypted=%v", pass, IsEncrypted(raw))
		}
		if pass != "" && (bytes.Contains(raw, []byte("KEY")) || bytes.Contains(raw, []byte("PK\x03\x04"))) {
			t.Fatal("encrypted backup shows plaintext")
		}
		st.Close()

		target := StateDirLayout(t.TempDir())
		o, err := OpenFile(p, pass)
		if err != nil {
			t.Fatal(err)
		}
		rep, skipped, err := Restore(o, target, false)
		o.Close()
		if err != nil || len(skipped) != 0 {
			t.Fatalf("restore: %v %v", err, skipped)
		}
		if rep.DB.Jobs != 20 || rep.DB.SchemaVersion != persistent.CurrentSchema || !rep.Manifest.Artifacts {
			t.Fatalf("report %+v", rep)
		}
		for name, want := range map[string]string{
			target.OperatorToken: "optoken", target.PairingToken: "pair-by-value",
			filepath.Join(target.TLSDir, "manager-key.pem"): "KEY",
			filepath.Join(target.ArtifactDir, "sha256", "ab", strings.Repeat("ab", 32)): "file body",
		} {
			if got, _ := os.ReadFile(name); string(got) != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if _, err := os.Stat(filepath.Join(target.ArtifactDir, "tmp", "upload-1")); err == nil {
			t.Error("half-written upload was backed up")
		}
		s2, err := persistent.Open(target.DB)
		if err != nil {
			t.Fatal(err)
		}
		if jobs, _ := s2.ListJobs(); len(jobs) != 20 {
			t.Errorf("restored db has %d jobs", len(jobs))
		}
		s2.Close()
		_ = l
	}
}

func TestVerifyCatchesDamage(t *testing.T) {
	_, src, _ := makeState(t, false)
	p := writeBackup(t, src, "")
	raw, _ := os.ReadFile(p)

	// A flipped byte in the middle of the file.
	bad := append([]byte{}, raw...)
	bad[len(bad)/3] ^= 0xff
	badPath := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(badPath, bad, 0o600)
	if o, err := OpenFile(badPath, ""); err == nil {
		_, err = Verify(o)
		o.Close()
		if err == nil {
			t.Error("a damaged backup verified")
		}
	}

	// Truncation.
	os.WriteFile(badPath, raw[:len(raw)/2], 0o600)
	if o, err := OpenFile(badPath, ""); err == nil {
		if _, err = Verify(o); err == nil {
			t.Error("a truncated backup verified")
		}
		o.Close()
	}
}

func TestEncryptedBackupRejectsWrongPassphraseAndTampering(t *testing.T) {
	_, src, _ := makeState(t, false)
	p := writeBackup(t, src, "right")
	if _, err := OpenFile(p, "wrong"); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := OpenFile(p, ""); !errors.Is(err, ErrEncrypted) {
		t.Errorf("no passphrase: %v", err)
	}
	raw, _ := os.ReadFile(p)
	dir := t.TempDir()
	for name, mut := range map[string]func([]byte) []byte{
		"cut":    func(b []byte) []byte { return b[:len(b)-20] },
		"flip":   func(b []byte) []byte { b[len(b)-20] ^= 1; return b },
		"header": func(b []byte) []byte { b[len(encMagic)+1] ^= 1; return b },
	} {
		f := filepath.Join(dir, name)
		os.WriteFile(f, mut(append([]byte{}, raw...)), 0o600)
		o, err := OpenFile(f, "right")
		if err == nil {
			o.Close()
			t.Errorf("%s: opened", name)
		}
	}
}

func TestEncryptStreamsAcrossChunkBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, encChunk - 1, encChunk, encChunk + 1, 3*encChunk + 7} {
		data := bytes.Repeat([]byte{7}, n)
		var buf bytes.Buffer
		w, err := NewEncryptWriter(&buf, "pw", testIter)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		w.Close()
		r, err := NewDecryptReader(&buf, "pw")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: %v (got %d bytes)", n, err, len(got))
		}
	}
}

func TestRestoreRefusesALiveManagerAndExistingState(t *testing.T) {
	_, src, st := makeState(t, false)
	p := writeBackup(t, src, "")
	o, _ := OpenFile(p, "")
	defer o.Close()

	// The source's own database is open in this process, as a manager's would be.
	live := StateDirLayout(filepath.Dir(src.Layout.DB))
	if _, _, err := Restore(o, live, true); !errors.Is(err, ErrRunning) {
		t.Fatalf("over a live manager: %v", err)
	}
	st.Close()
	if _, _, err := Restore(o, live, false); !errors.Is(err, ErrExists) {
		t.Fatalf("over existing state without force: %v", err)
	}
	if _, _, err := Restore(o, live, true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if _, err := os.Stat(live.DB + ".pre-restore"); err != nil {
		t.Error("the replaced database was not kept")
	}
}

func TestRestoreRefusesNewerDatabaseAndForeignPaths(t *testing.T) {
	// A hand-made backup whose manifest lists a path escaping the state.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil")
	w.Write([]byte("x"))
	m, _ := json.Marshal(Manifest{Format: 1, Entries: []Entry{{Name: "../evil", Size: 1, SHA256: "00"}}})
	w, _ = zw.Create(entryManifest)
	w.Write(m)
	zw.Close()
	p := filepath.Join(t.TempDir(), "evil.zip")
	os.WriteFile(p, buf.Bytes(), 0o600)
	o, _ := OpenFile(p, "")
	defer o.Close()
	if _, err := Verify(o); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("path escape: %v", err)
	}

	// An export-state zip has no manifest.
	buf.Reset()
	zw = zip.NewWriter(&buf)
	zw.Create("manager.db")
	zw.Close()
	os.WriteFile(p, buf.Bytes(), 0o600)
	o2, _ := OpenFile(p, "")
	defer o2.Close()
	if _, err := Verify(o2); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Fatalf("no manifest: %v", err)
	}
}

func TestScheduleKeepsNewestChecksAndPrunes(t *testing.T) {
	_, src, _ := makeState(t, false)
	dir := t.TempDir()
	s := &Schedule{Dir: dir, Keep: 2, Iterations: testIter, Make: func(w io.Writer) error { _, err := Create(w, src); return err }}
	var last string
	for i := 0; i < 4; i++ {
		p, err := s.RunOnce()
		if err != nil {
			t.Fatal(err)
		}
		last = p
	}
	files, _ := filepath.Glob(filepath.Join(dir, filePrefix+"*"))
	if len(files) != 2 || files[1] != last {
		t.Fatalf("kept %v, newest %s", files, last)
	}
	if l := s.LastResult(); !l.OK || l.Bytes == 0 {
		t.Fatalf("last = %+v", l)
	}

	// A backup that cannot be written (the source fails, as with a full
	// disk) leaves the existing ones, and no partial file, behind.
	s.Make = func(w io.Writer) error { w.Write([]byte("partial")); return errors.New("no space left on device") }
	if _, err := s.RunOnce(); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(after) != 2 {
		t.Fatalf("a failed backup left files behind: %v", after)
	}
	if l := s.LastResult(); l.OK || l.Error == "" {
		t.Fatalf("last = %+v", l)
	}
}

func TestEncryptedScheduleUsesSealedExtension(t *testing.T) {
	_, src, _ := makeState(t, false)
	s := &Schedule{Dir: t.TempDir(), Keep: 1, Passphrase: "pw", Iterations: testIter, Make: func(w io.Writer) error { _, err := Create(w, src); return err }}
	p, err := s.RunOnce()
	if err != nil || filepath.Ext(p) != extSealed {
		t.Fatalf("%s %v", p, err)
	}
}
