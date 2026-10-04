package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

// startTestManagerWithDashboard is startManagerWithAgentBinary
// (selfupdate_test.go) plus an httptest server wrapping the real HTTP API
// mux, used by both the dashboard smoke test and the /join-script tests
// below — they all just need a manager with a known Config, no real
// agent connections.
func startTestManagerWithDashboard(t *testing.T, addr string, cfg manager.Config) (*manager.Server, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	transport := ws.New()
	cfg.Addr = addr
	srv := manager.NewServer(transport, nil, cfg)
	if len(cfg.AgentBinaries) > 0 {
		transport.Handle("/agent-binary", srv.AgentBinaryHandler())
	}
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()
	waitListening(t, addr)

	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(apiSrv.Close)
	return srv, apiSrv
}

// TestDashboardServesHTML is a light smoke test — this project has no
// browser-based test tooling, so it just confirms the embedded page is
// wired up and served with the right content type, not that the JS
// behaves correctly (verified manually per the v5 part 2 plan).
func TestDashboardServesHTML(t *testing.T) {
	_, apiSrv := startTestManagerWithDashboard(t, "127.0.0.1:19310", manager.Config{
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})

	resp, err := http.Get(apiSrv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html content type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<title>Home Compute Harness</title>") {
		t.Fatal("expected the dashboard's <title> in the served page")
	}
	if !strings.Contains(string(body), "Create QR + link") {
		t.Fatal("expected QR enrollment control in dashboard")
	}
	for _, want := range []string{`data-revoke=`, `"/revocations"`, `"/revoke"`, `Allow re-enrollment`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("expected node revocation controls in dashboard (missing %q)", want)
		}
	}
}

// TestJoinScriptEndpointReturnsGeneratedScript proves GET /join-script
// produces the same script cmd/harnessctl's `join` command would, via the
// real HTTP API — both call the shared internal/joinscript.Build — and
// that with several builds loaded, each platform's script downloads and
// verifies its own build.
func TestJoinScriptEndpointReturnsGeneratedScript(t *testing.T) {
	windowsPath, windowsHash := writeDummyAgentBinary(t, []byte("windows build"))
	androidPath, wantHash := writeDummyAgentBinary(t, []byte("android build"))
	_, apiSrv := startTestManagerWithDashboard(t, "127.0.0.1:19311", manager.Config{
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
		AgentBinaries: []manager.AgentBinary{
			{OS: "windows", Arch: "amd64", Path: windowsPath},
			{OS: "linux", Arch: "arm64", Path: androidPath},
		},
		Fingerprint: "test-fingerprint",
	})

	resp, err := http.Get(apiSrv.URL + "/join-script?addr=192.168.10.11:7420&platform=android")
	if err != nil {
		t.Fatalf("GET /join-script: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	script := string(body)

	for _, want := range []string{
		"192.168.10.11:7420",
		"test-fingerprint",
		pairingToken,
		strings.ToLower(wantHash),
		"https://192.168.10.11:7420/agent-binaries/linux/arm64",
		"termux-wake-lock",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("expected generated script to contain %q, got:\n%s", want, script)
		}
	}
	if strings.Contains(strings.ToLower(script), strings.ToLower(windowsHash)) {
		t.Errorf("android script must not reference the Windows build's hash, got:\n%s", script)
	}

	resp, err = http.Get(apiSrv.URL + "/join-script?addr=192.168.10.11:7420&platform=windows")
	if err != nil {
		t.Fatalf("GET /join-script windows: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), strings.ToUpper(windowsHash)) || !strings.Contains(string(body), "/agent-binaries/windows/amd64") {
		t.Errorf("expected the windows script to use the Windows build, got:\n%s", body)
	}
}

// An Android script on a manager with only a Windows build must be refused
// outright: generating one would download the Windows binary and verify it
// against its own (matching) hash, installing an agent that can't run.
func TestJoinScriptRefusesPlatformWithoutBuild(t *testing.T) {
	binaryPath, _ := writeDummyAgentBinary(t, []byte("windows only"))
	_, apiSrv := startTestManagerWithDashboard(t, "127.0.0.1:19504", manager.Config{
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
		AgentBinaries:    dummyWindowsBuild(binaryPath),
		Fingerprint:      "test-fingerprint",
	})
	resp, err := http.Get(apiSrv.URL + "/join-script?addr=192.168.10.11:7420&platform=android")
	if err != nil {
		t.Fatalf("GET /join-script: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a platform with no build loaded, got %d", resp.StatusCode)
	}
}

// TestJoinScriptEndpointRejectsBadInput proves validation errors from
// joinscript.Build surface as 400s over the real API, not 500s or silent
// empty responses.
func TestJoinScriptEndpointRejectsBadInput(t *testing.T) {
	_, apiSrv := startTestManagerWithDashboard(t, "127.0.0.1:19312", manager.Config{
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})

	cases := []string{
		"/join-script?addr=no-port&platform=windows",
		"/join-script?addr=192.168.10.11:7420&platform=ios",
		"/join-script?addr=192.168.10.11:7420&platform=windows", // no -agent-binary configured
	}
	for _, path := range cases {
		resp, err := http.Get(apiSrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: expected 400, got %d", path, resp.StatusCode)
		}
	}
}

func TestJoinScriptEndpointReturnsRemoteRelayScript(t *testing.T) {
	binaryPath, _ := writeDummyAgentBinary(t, []byte("content"))
	_, apiSrv := startTestManagerWithDashboard(t, "127.0.0.1:19313", manager.Config{
		PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second,
		AgentBinaries: dummyWindowsBuild(binaryPath), Fingerprint: "test-fingerprint",
		RelayAddr: "relay.example.com:8420", RelayToken: "relay-secret",
	})

	resp, err := http.Get(apiSrv.URL + "/join-script?mode=remote&platform=windows")
	if err != nil {
		t.Fatalf("GET remote /join-script: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	script := string(body)
	for _, want := range []string{"relay.example.com:8420", "relay-secret", "test-fingerprint", pairingToken} {
		if !strings.Contains(script, want) {
			t.Errorf("expected remote script to contain %q, got:\n%s", want, script)
		}
	}
	if strings.Contains(script, "curl") || strings.Contains(script, "-manager-addr") {
		t.Errorf("remote script should not attempt an unsupported direct download/dial, got:\n%s", script)
	}
}
