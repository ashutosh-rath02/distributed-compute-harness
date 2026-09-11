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

func TestCmdJoinSecureEmbedsFingerprintTokenAndHash(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"abc123","pairingToken":"secret-token","insecure":false,"agentBinaryAvailable":true,"agentBinarySha256":"deadbeef"}`)

	out := captureStdout(t, func() {
		if err := c.cmdJoin("192.168.10.11:7420"); err != nil {
			t.Fatalf("cmdJoin: %v", err)
		}
	})

	for _, want := range []string{
		"curl.exe -k ",
		`https://192.168.10.11:7420/agent-binary`,
		"DEADBEEF", // Get-FileHash renders uppercase hex
		"-manager-addr 192.168.10.11:7420",
		"-pairing-token secret-token",
		"-manager-fingerprint abc123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "-insecure") {
		t.Errorf("expected no -insecure flag in secure mode, got:\n%s", out)
	}
	// The hash check and the launch must be one if/else statement, not two
	// sequential lines — pasted into an interactive PowerShell session,
	// separate lines each run independently, so a bare "if { throw }"
	// followed by a launch line would still launch after the throw merely
	// printed an error. Assert the launch is textually inside the else
	// branch (same line as "} else {"), not just present somewhere in the
	// output.
	if !strings.Contains(out, `} else { .\agent.exe`) {
		t.Errorf("expected the agent.exe launch inside the hash check's else branch, got:\n%s", out)
	}
}

func TestCmdJoinInsecureUsesHTTPAndInsecureFlag(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"","pairingToken":"secret-token","insecure":true,"agentBinaryAvailable":true,"agentBinarySha256":"deadbeef"}`)

	out := captureStdout(t, func() {
		if err := c.cmdJoin("192.168.10.11:7420"); err != nil {
			t.Fatalf("cmdJoin: %v", err)
		}
	})

	if !strings.Contains(out, "http://192.168.10.11:7420/agent-binary") {
		t.Errorf("expected an http:// download URL in insecure mode, got:\n%s", out)
	}
	if strings.Contains(out, "curl.exe -k ") {
		t.Errorf("expected no cert-skip flag in insecure (plaintext) mode, got:\n%s", out)
	}
	if !strings.Contains(out, "-insecure") {
		t.Errorf("expected -insecure flag in insecure mode, got:\n%s", out)
	}
	if strings.Contains(out, "-manager-fingerprint") {
		t.Errorf("expected no -manager-fingerprint flag in insecure mode, got:\n%s", out)
	}
}

func TestCmdJoinFailsWhenAgentBinaryNotConfigured(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"abc123","pairingToken":"secret-token","insecure":false,"agentBinaryAvailable":false}`)

	err := c.cmdJoin("192.168.10.11:7420")
	if err == nil {
		t.Fatal("expected an error when the manager has no agent binary configured")
	}
}

func TestCmdJoinRejectsAddressWithoutPort(t *testing.T) {
	c := &apiClient{base: "http://unused"}
	if err := c.cmdJoin("192.168.10.11"); err == nil {
		t.Fatal("expected an error for an address missing a port")
	}
}

func TestCmdJoinRejectsAddressWithoutHost(t *testing.T) {
	c := &apiClient{base: "http://unused"}
	// ":7420" is the manager's own -addr default and startup log text — an
	// operator copying it literally must be rejected, not sent a broken
	// "https://:7420/agent-binary" download URL.
	if err := c.cmdJoin(":7420"); err == nil {
		t.Fatal("expected an error for an address missing a host")
	}
}
