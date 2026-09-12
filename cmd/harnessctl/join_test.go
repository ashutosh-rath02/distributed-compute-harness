package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it wrote — cmdJoin prints its result rather than returning it,
// since it's meant to be copy-pasted straight out of a terminal.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	fn()

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(out)
}

func joinInfoServer(t *testing.T, body string) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/join-info" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &apiClient{base: srv.URL}
}

// The actual script content/format is internal/joinscript's responsibility
// and tested there (joinscript_test.go) — these tests only confirm cmdJoin
// wires /join-info's response into joinscript.Build correctly and prints
// or surfaces exactly what Build returns.

func TestCmdJoinPrintsBuildsOutputOnSuccess(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"abc123","pairingToken":"secret-token","insecure":false,"agentBinaryAvailable":true,"agentBinarySha256":"deadbeef"}`)

	out := captureStdout(t, func() {
		if err := c.cmdJoin("192.168.10.11:7420", "windows"); err != nil {
			t.Fatalf("cmdJoin: %v", err)
		}
	})

	// Spot-check a couple of substitutions made it through the /join-info
	// -> joinscript.Info -> Build pipeline intact; joinscript_test.go
	// covers the full script format exhaustively.
	if !strings.Contains(out, "192.168.10.11:7420") || !strings.Contains(out, "secret-token") || !strings.Contains(out, "abc123") {
		t.Errorf("expected the join-info fields to reach the generated script, got:\n%s", out)
	}
}

func TestCmdJoinSurfacesBuildValidationErrors(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"abc123","pairingToken":"secret-token","insecure":false,"agentBinaryAvailable":false}`)

	if err := c.cmdJoin("192.168.10.11:7420", "windows"); err == nil {
		t.Fatal("expected cmdJoin to surface joinscript.Build's agent-binary-unavailable error")
	}
	if err := c.cmdJoin("no-port-here", "windows"); err == nil {
		t.Fatal("expected cmdJoin to surface joinscript.Build's bad-address error")
	}
	if err := c.cmdJoin("192.168.10.11:7420", "ios"); err == nil {
		t.Fatal("expected cmdJoin to surface joinscript.Build's unknown-platform error")
	}
}
