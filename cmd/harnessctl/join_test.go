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
		if err := c.cmdJoin("192.168.10.11:7420", "windows"); err != nil {
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
		if err := c.cmdJoin("192.168.10.11:7420", "windows"); err != nil {
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

	err := c.cmdJoin("192.168.10.11:7420", "windows")
	if err == nil {
		t.Fatal("expected an error when the manager has no agent binary configured")
	}
}

func TestCmdJoinRejectsAddressWithoutPort(t *testing.T) {
	c := &apiClient{base: "http://unused"}
	if err := c.cmdJoin("192.168.10.11", "windows"); err == nil {
		t.Fatal("expected an error for an address missing a port")
	}
}

func TestCmdJoinRejectsAddressWithoutHost(t *testing.T) {
	c := &apiClient{base: "http://unused"}
	// ":7420" is the manager's own -addr default and startup log text — an
	// operator copying it literally must be rejected, not sent a broken
	// "https://:7420/agent-binary" download URL.
	if err := c.cmdJoin(":7420", "windows"); err == nil {
		t.Fatal("expected an error for an address missing a host")
	}
}

func TestCmdJoinRejectsUnknownPlatform(t *testing.T) {
	c := &apiClient{base: "http://unused"}
	if err := c.cmdJoin("192.168.10.11:7420", "ios"); err == nil {
		t.Fatal("expected an error for an unsupported platform")
	}
}

func TestCmdJoinAndroidEmbedsTermuxSetupAndHash(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"abc123","pairingToken":"secret-token","insecure":false,"agentBinaryAvailable":true,"agentBinarySha256":"DEADBEEF"}`)

	out := captureStdout(t, func() {
		if err := c.cmdJoin("192.168.10.11:7420", "android"); err != nil {
			t.Fatalf("cmdJoin: %v", err)
		}
	})

	for _, want := range []string{
		"f-droid.org/packages/com.termux",
		"f-droid.org/packages/com.termux.boot",
		"pkg install -y curl",
		`curl -k -o agent "https://192.168.10.11:7420/agent-binary"`,
		"deadbeef", // sha256sum renders lowercase hex, unlike PowerShell's Get-FileHash
		"~/.termux/boot/start-harness-agent.sh",
		"termux-wake-lock",
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
	// Same lesson as the PowerShell if/else fix, applied to bash: the hash
	// check must gate the download's use via && chaining, not sit on its
	// own line, or a failed check wouldn't stop a separate next line from
	// writing the boot script and launching the unverified binary anyway.
	if !strings.Contains(out, `] && chmod +x agent`) {
		t.Errorf("expected the hash check chained with && into the rest of the setup, got:\n%s", out)
	}
	// A bare trailing "... & disown" parses as TWO commands, not one: bash
	// backgrounds everything up to "&" as its own job, then runs "disown"
	// separately — silently breaking the whole && chain (pkg install,
	// curl, the hash check) out of the foreground, discarding any failure
	// output. The launch must be backgrounded *inside* the chain (e.g.
	// "(cmd &)") with something observable chained after it.
	if strings.Contains(out, "& disown") {
		t.Errorf("expected no bare '& disown' (breaks the && chain into a background job), got:\n%s", out)
	}
	if !strings.Contains(out, `&& echo "Installed`) {
		t.Errorf("expected a chained success message after the launch, got:\n%s", out)
	}
}

func TestCmdJoinAndroidInsecureUsesHTTPAndInsecureFlag(t *testing.T) {
	c := joinInfoServer(t, `{"fingerprint":"","pairingToken":"secret-token","insecure":true,"agentBinaryAvailable":true,"agentBinarySha256":"deadbeef"}`)

	out := captureStdout(t, func() {
		if err := c.cmdJoin("192.168.10.11:7420", "android"); err != nil {
			t.Fatalf("cmdJoin: %v", err)
		}
	})

	if !strings.Contains(out, `curl -o agent "http://192.168.10.11:7420/agent-binary"`) {
		t.Errorf("expected a plain http curl with no -k flag in insecure mode, got:\n%s", out)
	}
	if !strings.Contains(out, "-insecure") {
		t.Errorf("expected -insecure flag in insecure mode, got:\n%s", out)
	}
	if strings.Contains(out, "-manager-fingerprint") {
		t.Errorf("expected no -manager-fingerprint flag in insecure mode, got:\n%s", out)
	}
}
