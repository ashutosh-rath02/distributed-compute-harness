package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.bin")
	content := []byte("hello self-update")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("hashFile = %q, want %q", got, want)
	}
}

func TestHashFileMissingFileFails(t *testing.T) {
	if _, err := hashFile(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected an error hashing a nonexistent file")
	}
}

// TestApplySelfUpdateSwapsFileIntoPlace proves the core rename dance
// against real files standing in for the exe path — never the actual
// running test binary, which would be reckless to rename mid-test.
func TestApplySelfUpdateSwapsFileIntoPlace(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "agent.exe")
	downloadPath := exePath + selfUpdateDownloadSuffix
	oldPath := exePath + selfUpdateOldSuffix

	if err := os.WriteFile(exePath, []byte("old binary"), 0o644); err != nil {
		t.Fatalf("WriteFile exePath: %v", err)
	}
	if err := os.WriteFile(downloadPath, []byte("new binary"), 0o644); err != nil {
		t.Fatalf("WriteFile downloadPath: %v", err)
	}

	if err := applySelfUpdate(exePath, downloadPath); err != nil {
		t.Fatalf("applySelfUpdate: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile exePath after update: %v", err)
	}
	if string(got) != "new binary" {
		t.Fatalf("expected exePath to contain the new binary's content, got %q", got)
	}
	if _, err := os.Stat(downloadPath); !os.IsNotExist(err) {
		t.Fatalf("expected the .download file to be gone after a successful update, stat err: %v", err)
	}
	// oldPath is removed best-effort on success; either gone or (if the
	// best-effort remove happened to fail) still holding the old content —
	// both are acceptable, but it must never hold the NEW content.
	if oldContent, err := os.ReadFile(oldPath); err == nil && string(oldContent) != "old binary" {
		t.Fatalf("expected .old (if still present) to hold the previous binary's content, got %q", oldContent)
	}
}

// newTestAgent builds a real *Agent (real identity, loaded into a fresh
// temp dir) purely for exercising selfupdate.go's methods — Run is never
// called, so the transport is never dialed and can be nil.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New(nil, Config{IdentityDir: t.TempDir(), Insecure: true, LaunchArgs: []string{"-pairing-token", "test"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// withStubRelaunch swaps the package-level relaunch var for a
// capturing/no-op stub for the duration of a test, restoring the real one
// afterward. Every test that calls performSelfUpdateAt must use this —
// the real relaunch execs a process and calls os.Exit, which would end the
// test binary itself.
func withStubRelaunch(t *testing.T) *[]struct {
	exePath string
	args    []string
} {
	t.Helper()
	var mu sync.Mutex
	calls := &[]struct {
		exePath string
		args    []string
	}{}
	original := relaunch
	relaunch = func(exePath string, args []string, logPath string) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, struct {
			exePath string
			args    []string
		}{exePath, args})
	}
	t.Cleanup(func() { relaunch = original })
	return calls
}

// TestPerformSelfUpdateAtFullFlow exercises the real download (over a real
// local HTTP server), hash verification, and file swap end to end — using
// a throwaway temp file as the "executable," never the actual test binary,
// and a stubbed relaunch so the test process itself never execs/exits.
func TestPerformSelfUpdateAtFullFlow(t *testing.T) {
	newContent := []byte("new agent binary content")
	sum := sha256.Sum256(newContent)
	wantHash := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/agent-binary", func(w http.ResponseWriter, r *http.Request) {
		w.Write(newContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	a := newTestAgent(t)
	a.setCurrentManagerAddr(strings.TrimPrefix(server.URL, "http://"))
	calls := withStubRelaunch(t)

	exePath := filepath.Join(t.TempDir(), "agent.exe")
	if err := os.WriteFile(exePath, []byte("old agent binary content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a.performSelfUpdateAt(exePath, wantHash)

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile after update: %v", err)
	}
	if string(got) != string(newContent) {
		t.Fatalf("expected exePath to hold the new content, got %q", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 relaunch call, got %d", len(*calls))
	}
	if (*calls)[0].exePath != exePath {
		t.Fatalf("expected relaunch called with %q, got %q", exePath, (*calls)[0].exePath)
	}
	if len((*calls)[0].args) != 2 || (*calls)[0].args[0] != "-pairing-token" {
		t.Fatalf("expected relaunch called with the original LaunchArgs, got %v", (*calls)[0].args)
	}
}

// TestPerformSelfUpdateAtHashMismatchLeavesBinaryUntouched proves a server
// response that doesn't match the expected hash never reaches the swap
// step at all — the old binary survives byte-for-byte and relaunch is
// never called.
func TestPerformSelfUpdateAtHashMismatchLeavesBinaryUntouched(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-binary", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("unexpected content"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	a := newTestAgent(t)
	a.setCurrentManagerAddr(strings.TrimPrefix(server.URL, "http://"))
	calls := withStubRelaunch(t)

	exePath := filepath.Join(t.TempDir(), "agent.exe")
	oldContent := []byte("old agent binary content")
	if err := os.WriteFile(exePath, oldContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a.performSelfUpdateAt(exePath, "0000000000000000000000000000000000000000000000000000000000000000")

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile after mismatched update: %v", err)
	}
	if string(got) != string(oldContent) {
		t.Fatalf("expected exePath untouched after a hash mismatch, got %q", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("expected relaunch never called on a hash mismatch, got %d calls", len(*calls))
	}
	if _, err := os.Stat(exePath + selfUpdateDownloadSuffix); !os.IsNotExist(err) {
		t.Fatalf("expected the .download file to be cleaned up, stat err: %v", err)
	}
}

func TestApplySelfUpdateFailsClosedWhenCurrentBinaryMissing(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "agent.exe") // never created
	downloadPath := exePath + selfUpdateDownloadSuffix
	if err := os.WriteFile(downloadPath, []byte("new binary"), 0o644); err != nil {
		t.Fatalf("WriteFile downloadPath: %v", err)
	}

	if err := applySelfUpdate(exePath, downloadPath); err == nil {
		t.Fatal("expected an error when the current binary doesn't exist to rename out of the way")
	}
	if _, err := os.Stat(downloadPath); !os.IsNotExist(err) {
		t.Fatalf("expected the .download file to be cleaned up on this failure path, stat err: %v", err)
	}
}
