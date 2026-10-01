package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/domain"
)

type fakeTransfer struct {
	mu       sync.Mutex
	inputs   map[string][]byte // sha -> content
	fetchErr error
	uploaded map[string][]byte // name -> content
}

func (f *fakeTransfer) Fetch(_ context.Context, ref domain.ArtifactRef, dest string) error {
	if f.fetchErr != nil {
		return f.fetchErr
	}
	return os.WriteFile(dest, f.inputs[ref.SHA256], 0o600)
}

func (f *fakeTransfer) Upload(_ context.Context, name, src string) (domain.ArtifactRef, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploaded == nil {
		f.uploaded = map[string][]byte{}
	}
	f.uploaded[name] = b
	return domain.ArtifactRef{Name: name, SHA256: shaOf(b), Size: int64(len(b))}, nil
}

func shaOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

const fileWorkloadID = domain.WorkloadID("0123456789abcdef0123456789abcdef")

// copyWorkload copies in/data.txt to out/result.txt inside the work dir.
func copyWorkload(input []byte, outputs ...string) domain.Workload {
	wl := domain.Workload{ID: fileWorkloadID, Inputs: []domain.ArtifactRef{{Name: "in/data.txt", SHA256: shaOf(input), Size: int64(len(input))}}, Outputs: outputs}
	if runtime.GOOS == "windows" {
		wl.Command, wl.Args = "cmd", []string{"/C", `copy /Y in\data.txt out\result.txt`}
	} else {
		wl.Command, wl.Args = "cp", []string{"in/data.txt", "out/result.txt"}
	}
	return wl
}

func runFiles(t *testing.T, e *Executor, wl domain.Workload, xfer ArtifactTransfer) domain.WorkloadStatus {
	t.Helper()
	done := make(chan domain.WorkloadStatus, 1)
	if err := e.StartWithFiles(context.Background(), wl, xfer, func(s domain.WorkloadStatus) {
		if s.State != domain.WorkloadRunning {
			done <- s
		}
	}); err != nil {
		t.Fatalf("StartWithFiles: %v", err)
	}
	select {
	case s := <-done:
		return s
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the file workload to finish")
		return domain.WorkloadStatus{}
	}
}

func fileExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	e := NewExecutor()
	e.SetWorkRoot(root)
	return e, root
}

func TestFileWorkloadGetsInputsAndUploadsOutputs(t *testing.T) {
	e, root := fileExecutor(t)
	input := []byte("hello from the manager\n")
	xfer := &fakeTransfer{inputs: map[string][]byte{shaOf(input): input}}
	st := runFiles(t, e, copyWorkload(input, "out/result.txt"), xfer)
	if st.State != domain.WorkloadCompleted {
		t.Fatalf("got %s (%s; stderr %q)", st.State, st.Error, st.Stderr)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].Name != "out/result.txt" || st.Outputs[0].SHA256 != shaOf(input) {
		t.Fatalf("outputs = %+v", st.Outputs)
	}
	if string(xfer.uploaded["out/result.txt"]) != string(input) {
		t.Fatalf("uploaded %q", xfer.uploaded["out/result.txt"])
	}
	if st.StartedAt.IsZero() {
		t.Fatal("StartedAt must be set")
	}
	if _, err := os.Stat(filepath.Join(root, string(fileWorkloadID))); !os.IsNotExist(err) {
		t.Fatal("the working directory must be removed afterwards")
	}
	if e.isCanceled(fileWorkloadID) || len(e.running) != 0 {
		t.Fatal("the slot must be released")
	}
}

func TestMissingDeclaredOutputFailsAndUploadsNothing(t *testing.T) {
	e, _ := fileExecutor(t)
	input := []byte("x")
	xfer := &fakeTransfer{inputs: map[string][]byte{shaOf(input): input}}
	st := runFiles(t, e, copyWorkload(input, "out/result.txt", "never-made.txt"), xfer)
	if st.State != domain.WorkloadFailed || !strings.Contains(st.Error, "never-made.txt") {
		t.Fatalf("got %s (%s), want FAILED naming the missing output", st.State, st.Error)
	}
	if len(st.Outputs) != 0 {
		t.Fatalf("a failed run must report no outputs, got %+v", st.Outputs)
	}
}

func TestFailedRunUploadsNothing(t *testing.T) {
	e, _ := fileExecutor(t)
	xfer := &fakeTransfer{}
	wl := domain.Workload{ID: fileWorkloadID, Outputs: []string{"out.txt"}}
	if runtime.GOOS == "windows" {
		wl.Command, wl.Args = "cmd", []string{"/C", "echo partial> out.txt & exit 3"}
	} else {
		wl.Command, wl.Args = "sh", []string{"-c", "echo partial > out.txt; exit 3"}
	}
	st := runFiles(t, e, wl, xfer)
	if st.State != domain.WorkloadFailed || st.ExitCode != 3 {
		t.Fatalf("got %s exit %d, want FAILED exit 3", st.State, st.ExitCode)
	}
	if len(xfer.uploaded) != 0 || len(st.Outputs) != 0 {
		t.Fatal("a failed run must upload nothing")
	}
}

func TestInputFetchFailureFailsBeforeRunning(t *testing.T) {
	e, _ := fileExecutor(t)
	xfer := &fakeTransfer{fetchErr: errors.New("manager unreachable")}
	var sawRunning bool
	done := make(chan domain.WorkloadStatus, 1)
	err := e.StartWithFiles(context.Background(), copyWorkload([]byte("x"), "out/result.txt"), xfer, func(s domain.WorkloadStatus) {
		if s.State == domain.WorkloadRunning {
			sawRunning = true
			return
		}
		done <- s
	})
	if err != nil {
		t.Fatal(err)
	}
	st := <-done
	if st.State != domain.WorkloadFailed || !strings.Contains(st.Error, "manager unreachable") || sawRunning {
		t.Fatalf("got %s (%s), running=%v", st.State, st.Error, sawRunning)
	}
	// A fetch failure is a real failure for the restart policy, not a
	// placement-time refusal.
	if st.StartedAt.IsZero() {
		t.Fatal("StartedAt must be set on a fetch failure")
	}
}

func TestCanceledFileWorkloadUploadsNothingAndCleansUp(t *testing.T) {
	e, root := fileExecutor(t)
	xfer := &fakeTransfer{}
	wl := domain.Workload{ID: fileWorkloadID, Outputs: []string{"out.txt"}}
	if runtime.GOOS == "windows" {
		wl.Command, wl.Args = "powershell", []string{"-NoProfile", "-Command", "Set-Content out.txt x; Start-Sleep -Seconds 30"}
	} else {
		wl.Command, wl.Args = "sh", []string{"-c", "echo x > out.txt; sleep 30"}
	}
	running := make(chan struct{}, 1)
	done := make(chan domain.WorkloadStatus, 1)
	if err := e.StartWithFiles(context.Background(), wl, xfer, func(s domain.WorkloadStatus) {
		if s.State == domain.WorkloadRunning {
			running <- struct{}{}
			return
		}
		done <- s
	}); err != nil {
		t.Fatal(err)
	}
	<-running
	time.Sleep(300 * time.Millisecond)
	if err := e.Cancel(fileWorkloadID); err != nil {
		t.Fatal(err)
	}
	var st domain.WorkloadStatus
	select {
	case st = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the canceled workload")
	}
	if st.State != domain.WorkloadCanceled || len(xfer.uploaded) != 0 {
		t.Fatalf("got %s with %d uploads, want CANCELED with none", st.State, len(xfer.uploaded))
	}
	waitGone := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, string(fileWorkloadID))); os.IsNotExist(err) {
			break
		}
		if time.Now().After(waitGone) {
			t.Fatal("the working directory must be removed after a cancel")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFileWorkloadRefusedWithoutTransferOrValidID(t *testing.T) {
	e, _ := fileExecutor(t)
	if st := runFiles(t, e, copyWorkload([]byte("x"), "out/result.txt"), nil); st.State != domain.WorkloadFailed {
		t.Fatalf("no transfer: got %s", st.State)
	}
	wl := copyWorkload([]byte("x"), "out/result.txt")
	wl.ID = "../escape"
	if st := runFiles(t, e, wl, &fakeTransfer{}); st.State != domain.WorkloadFailed || !strings.Contains(st.Error, "working directory") {
		t.Fatalf("bad ID: got %s (%s)", st.State, st.Error)
	}
	noRoot := NewExecutor()
	if st := runFiles(t, noRoot, copyWorkload([]byte("x"), "out/result.txt"), &fakeTransfer{}); st.State != domain.WorkloadFailed {
		t.Fatalf("no work root: got %s", st.State)
	}
	fsRead := domain.Workload{ID: fileWorkloadID, Capability: domain.CapabilityFilesystemRead, Outputs: []string{"x"}}
	if st := runFiles(t, e, fsRead, &fakeTransfer{}); st.State != domain.WorkloadFailed {
		t.Fatalf("filesystem.read with files: got %s", st.State)
	}
}

func TestCleanWorkRootRemovesOnlyWorkloadDirs(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, string(fileWorkloadID))
	keep := filepath.Join(root, "not-a-workload")
	for _, d := range []string{stale, keep} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cleanWorkRoot(root)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale workload directory survived")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("cleanWorkRoot removed something that isn't a workload directory")
	}
}

func TestWithin(t *testing.T) {
	base := t.TempDir()
	if !within(filepath.Join(base, "id", "work"), filepath.Join(base, "id")) || !within(base, base) {
		t.Fatal("within missed a nested path")
	}
	if within(filepath.Join(base, "idx"), filepath.Join(base, "id")) || within(filepath.Join(base, "work"), filepath.Join(base, "id")) {
		t.Fatal("within matched a sibling")
	}
}

func waitFinal(t *testing.T, e *Executor, wl domain.Workload) domain.WorkloadStatus {
	t.Helper()
	done := make(chan domain.WorkloadStatus, 1)
	if err := e.Start(context.Background(), wl, func(s domain.WorkloadStatus) {
		if s.State != domain.WorkloadRunning {
			done <- s
		}
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case s := <-done:
		return s
	case <-time.After(20 * time.Second):
		t.Fatal("timed out")
		return domain.WorkloadStatus{}
	}
}

func TestTypedTaskRunsItsBuiltinHandler(t *testing.T) {
	e := NewExecutor()
	st := waitFinal(t, e, domain.Workload{ID: "t1", Capability: "cpu.burn", Params: map[string]string{"seconds": "1", "threads": "1"}})
	if st.State != domain.WorkloadCompleted || !strings.Contains(st.Stdout, "busy for 1s") {
		t.Fatalf("cpu.burn: %s %q %s", st.State, st.Stdout, st.Error)
	}
}

// The agent re-validates a typed assignment against its own catalog: a
// manager on another version can't make it misread one.
func TestTypedAssignmentNotMatchingThisAgentsCatalogIsRefused(t *testing.T) {
	e := NewExecutor()
	for name, wl := range map[string]domain.Workload{
		"unknown param":     {ID: "a", Capability: "system.identity", Params: map[string]string{"verbose": "true"}},
		"non-canonical":     {ID: "b", Capability: "cpu.burn", Params: map[string]string{"seconds": "01", "threads": "0"}},
		"missing defaults":  {ID: "c", Capability: "cpu.burn", Params: map[string]string{"seconds": "1"}},
		"different outputs": {ID: "d", Capability: "file.hash", Params: map[string]string{"algorithm": "sha256"}, Inputs: []domain.ArtifactRef{{Name: "x", SHA256: strings.Repeat("0", 64)}}, Outputs: []string{"other.txt"}},
	} {
		if st := waitFinal(t, e, wl); st.State != domain.WorkloadFailed || !strings.Contains(st.Error, "doesn't match this agent's") {
			t.Errorf("%s: %s %q", name, st.State, st.Error)
		}
	}
}

func TestDisabledCapabilityIsRefused(t *testing.T) {
	e := NewExecutor()
	e.SetDisabled([]domain.CapabilityName{domain.CapabilitySystemExecute})
	if st := waitFinal(t, e, echoWorkload("x", "hi")); st.State != domain.WorkloadFailed || !strings.Contains(st.Error, "disabled on this device") {
		t.Fatalf("disabled raw command: %s %q", st.State, st.Error)
	}
}

func TestTimeoutFailsARawCommandDistinctFromCancel(t *testing.T) {
	e := NewExecutor()
	wl := sleepWorkload("slow", "30")
	wl.TimeoutSeconds = 1
	start := time.Now()
	st := waitFinal(t, e, wl)
	if st.State != domain.WorkloadFailed || st.Error != "timed out after 1s" || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %s %q after %v", st.State, st.Error, time.Since(start))
	}
}
