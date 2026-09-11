package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// TestFilesystemReadReturnsFileContent is v4's headline case end to end: a
// real agent (whose manifest, via the real sysinfo package, now actually
// declares CapabilityFilesystemRead) is asked to invoke that capability
// against a real file, and the content comes back over the same
// WORKLOAD_STATUS/Stdout path system.execute already uses.
func TestFilesystemReadReturnsFileContent(t *testing.T) {
	const addr = "127.0.0.1:19280"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "capability-agent-a")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	path := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(path, []byte("hello from the real agent"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), "", nil, domain.CapabilityFilesystemRead, map[string]string{"path": path}, domain.ResourceRequirements{}, domain.RestartNever)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadCompleted
	})

	rec, _ := srv.Workloads.Get(wl.ID)
	if rec.Status.Stdout != "hello from the real agent" {
		t.Fatalf("expected file content in Stdout, got %q", rec.Status.Stdout)
	}
}

// TestSubmitWorkloadRejectsUndeclaredCapability proves the manager refuses
// to dispatch a capability no connected node declares, rather than sending
// a WORKLOAD_ASSIGN the agent would then have to reject itself. "camera.capture"
// is a capability name nothing in this project implements yet.
func TestSubmitWorkloadRejectsUndeclaredCapability(t *testing.T) {
	const addr = "127.0.0.1:19281"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "capability-agent-b")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := srv.SubmitWorkload(ctx, a.NodeID(), "", nil, "camera.capture", nil, domain.ResourceRequirements{}, domain.RestartNever); err == nil {
		t.Fatal("expected SubmitWorkload to reject an explicit target lacking the requested capability")
	}
	if _, err := srv.SubmitWorkload(ctx, "", "", nil, "camera.capture", nil, domain.ResourceRequirements{}, domain.RestartNever); err == nil {
		t.Fatal("expected SubmitWorkload to reject auto-placement when no node declares the requested capability")
	}
}
