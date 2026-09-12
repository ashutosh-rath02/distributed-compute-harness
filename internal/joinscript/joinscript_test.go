package joinscript

import (
	"strings"
	"testing"
)

func TestBuildWindowsSecureEmbedsFingerprintTokenAndHash(t *testing.T) {
	out, err := Build("192.168.10.11:7420", "windows", Info{
		Fingerprint: "abc123", PairingToken: "secret-token",
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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

func TestBuildWindowsInsecureUsesHTTPAndInsecureFlag(t *testing.T) {
	out, err := Build("192.168.10.11:7420", "windows", Info{
		PairingToken: "secret-token", Insecure: true,
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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

func TestBuildFailsWhenAgentBinaryNotConfigured(t *testing.T) {
	_, err := Build("192.168.10.11:7420", "windows", Info{
		Fingerprint: "abc123", PairingToken: "secret-token", AgentBinaryAvailable: false,
	})
	if err == nil {
		t.Fatal("expected an error when the manager has no agent binary configured")
	}
}

func TestBuildRejectsAddressWithoutPort(t *testing.T) {
	if _, err := Build("192.168.10.11", "windows", Info{AgentBinaryAvailable: true}); err == nil {
		t.Fatal("expected an error for an address missing a port")
	}
}

func TestBuildRejectsAddressWithoutHost(t *testing.T) {
	// ":7420" is the manager's own -addr default and startup log text — an
	// operator copying it literally must be rejected, not sent a broken
	// "https://:7420/agent-binary" download URL.
	if _, err := Build(":7420", "windows", Info{AgentBinaryAvailable: true}); err == nil {
		t.Fatal("expected an error for an address missing a host")
	}
}

func TestBuildRejectsUnknownPlatform(t *testing.T) {
	if _, err := Build("192.168.10.11:7420", "ios", Info{AgentBinaryAvailable: true}); err == nil {
		t.Fatal("expected an error for an unsupported platform")
	}
}

func TestBuildAndroidEmbedsTermuxSetupAndHash(t *testing.T) {
	out, err := Build("192.168.10.11:7420", "android", Info{
		Fingerprint: "abc123", PairingToken: "secret-token",
		AgentBinaryAvailable: true, AgentBinarySHA256: "DEADBEEF",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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

func TestBuildAndroidInsecureUsesHTTPAndInsecureFlag(t *testing.T) {
	out, err := Build("192.168.10.11:7420", "android", Info{
		PairingToken: "secret-token", Insecure: true,
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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
