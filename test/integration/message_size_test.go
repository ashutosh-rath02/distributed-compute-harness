package integration

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// Real messages exceed the WebSocket library's 32 KiB default read limit
// in both directions; a message over the limit drops the connection.
func TestLargeMessagesCrossTheConnectionBothWays(t *testing.T) {
	const addr = "127.0.0.1:19547"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "big-message-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	// Agent -> manager: a status carrying ~60 KB of stdout.
	cmd, args := "sh", []string{"-c", "head -c 60000 /dev/zero | tr '\\0' x"}
	if runtime.GOOS == "windows" {
		cmd, args = "powershell", []string{"-NoProfile", "-Command", "[Console]::Out.Write('x' * 60000)"}
	}
	code, out := postWorkload(t, m.api, map[string]any{"target": a.NodeID(), "command": cmd, "args": args})
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["id"].(string)
	var v struct {
		State  domain.WorkloadState `json:"state"`
		Stdout string               `json:"stdout"`
	}
	waitFor(t, 30*time.Second, func() bool { getJSON(t, m.api+"/workloads/"+id, &v); return v.State == domain.WorkloadCompleted })
	if len(v.Stdout) != 60000 {
		t.Fatalf("stdout arrived as %d bytes, want 60000", len(v.Stdout))
	}

	// Manager -> agent: an assignment naming 250 input files (a job's
	// reduce can name up to 256).
	sha := uploadArtifact(t, m.api, []byte("p"))
	var inputs []map[string]string
	for i := 0; i < 250; i++ {
		inputs = append(inputs, map[string]string{"name": fmt.Sprintf("part-%03d-%s", i, strings.Repeat("n", 50)), "sha256": sha})
	}
	ecmd, eargs := echoArgs("many-inputs")
	code, out = postWorkload(t, m.api, map[string]any{"target": a.NodeID(), "command": ecmd, "args": eargs, "inputs": inputs})
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, out)
	}
	id = out["id"].(string)
	waitFor(t, 60*time.Second, func() bool {
		getJSON(t, m.api+"/workloads/"+id, &v)
		return v.State == domain.WorkloadCompleted || v.State == domain.WorkloadFailed
	})
	if v.State != domain.WorkloadCompleted {
		t.Fatalf("250-input workload: %s", v.State)
	}
}
