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
		"-pairing-token secret-token",
		"-manager-fingerprint abc123",
		`HKCU:\Software\Microsoft\Windows\CurrentVersion\Run`,
		"HomeComputeHarnessAgent",
		"start-agent.ps1",
		"Register-ScheduledTask",
		"-RunLevel Limited",
		"Remove-ItemProperty -Path $runKey",
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
	if strings.Contains(out, "-manager-addr") {
		t.Errorf("LAN installation must rediscover the manager instead of pinning its current IP, got:\n%s", out)
	}
	assertWindowsPersistentInstall(t, out)
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
	if strings.Contains(out, "-manager-addr") {
		t.Errorf("LAN Termux installation must rediscover the manager instead of pinning its current IP, got:\n%s", out)
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

func TestBuildRemoteWindowsUsesRelayAndNoDownload(t *testing.T) {
	out, err := BuildMode(ModeRemote, "", "windows", Info{
		Fingerprint: "abc123", PairingToken: "pair-secret",
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
		RelayAvailable: true, RelayAddr: "relay.example.com:8420", RelayToken: "relay-secret",
	})
	if err != nil {
		t.Fatalf("BuildMode: %v", err)
	}
	for _, want := range []string{
		"DEADBEEF", "-pairing-token pair-secret", "-manager-fingerprint abc123",
		"-relay-addr relay.example.com:8420", "-relay-token relay-secret",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "curl") || strings.Contains(out, "-manager-addr") {
		t.Errorf("remote script must not download from or directly dial the manager, got:\n%s", out)
	}
	assertWindowsPersistentInstall(t, out)
}

// The Windows relay paths once ran the agent in the foreground with no
// launcher, so a remote node died with its console window and never came
// back — unlike LAN Windows and both Android paths. Every Windows variant
// must install the same verified, per-user, logon-persistent launcher.
func assertWindowsPersistentInstall(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{
		`HKCU:\Software\Microsoft\Windows\CurrentVersion\Run`,
		"HomeComputeHarnessAgent",
		"start-agent.ps1",
		"Move-Item $download $agent",
		"Register-ScheduledTask",
		"-RestartCount 999",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected persistent install to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Index(out, "hash mismatch") > strings.Index(out, "Move-Item $download $agent") {
		t.Errorf("expected hash verification to precede installation, got:\n%s", out)
	}
	if strings.Contains(out, `.\agent.exe`) {
		t.Errorf("expected the installed launcher, not a foreground run of a local agent.exe, got:\n%s", out)
	}
}

func TestBuildRemoteWindowsBootstrapDownloadsAndInstallsLauncher(t *testing.T) {
	out, err := BuildMode(ModeRemote, "", "windows", Info{
		Fingerprint: "abc123", PairingToken: "one-time-token",
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
		RelayAvailable: true, RelayAddr: "relay.example.com:8420", RelayToken: "ha1.device-credential",
		BootstrapURL: "https://relay.example.com:8443/enroll/one-time-token/agent-binary",
	})
	if err != nil {
		t.Fatalf("BuildMode: %v", err)
	}
	for _, want := range []string{
		`curl.exe -fL "https://relay.example.com:8443/enroll/one-time-token/agent-binary" -o $download`,
		"DEADBEEF",
		"& $agent -pairing-token one-time-token -manager-fingerprint abc123 -relay-addr relay.example.com:8420 -relay-token ha1.device-credential",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "-manager-addr") {
		t.Errorf("remote script must not directly dial the manager, got:\n%s", out)
	}
	assertWindowsPersistentInstall(t, out)
}

func TestBuildRemoteAndroidUsesRelayAndBootScript(t *testing.T) {
	out, err := BuildMode(ModeRemote, "", "android", Info{
		Fingerprint: "abc123", PairingToken: "pair-secret",
		AgentBinaryAvailable: true, AgentBinarySHA256: "DEADBEEF",
		RelayAvailable: true, RelayAddr: "relay.example.com:8420", RelayToken: "relay-secret",
	})
	if err != nil {
		t.Fatalf("BuildMode: %v", err)
	}
	for _, want := range []string{"deadbeef", "termux-wake-lock", "-relay-addr relay.example.com:8420", "-relay-token relay-secret"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "curl") || strings.Contains(out, "-manager-addr") {
		t.Errorf("remote script must not download from or directly dial the manager, got:\n%s", out)
	}
}

func TestBuildRemoteRequiresRelayAndBinary(t *testing.T) {
	if _, err := BuildMode(ModeRemote, "", "windows", Info{AgentBinaryAvailable: true}); err == nil {
		t.Fatal("expected remote mode to require configured relay settings")
	}
	if _, err := BuildMode(ModeRemote, "", "windows", Info{
		RelayAvailable: true, RelayAddr: "relay.example.com:8420", RelayToken: "secret",
	}); err == nil {
		t.Fatal("expected remote mode to require a configured agent binary")
	}
}

func TestBuildRemoteRejectsUnusableRelayAddress(t *testing.T) {
	_, err := BuildMode(ModeRemote, "", "windows", Info{
		AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef",
		RelayAvailable: true, RelayAddr: ":8420", RelayToken: "secret",
	})
	if err == nil {
		t.Fatal("expected remote mode to reject a relay address without a host")
	}
}

func unixInfo() Info {
	return Info{
		Fingerprint: "abc123", PairingToken: "secret-token", AgentBinaryAvailable: true,
		ArchBuilds: map[string]ArchBuild{
			"arm64": {SHA256: "AAAA", Path: "/agent-binaries/darwin/arm64"},
			"amd64": {SHA256: "BBBB", Path: "/agent-binaries/darwin/amd64"},
		},
	}
}

func TestBuildMacOSPicksCPUAtInstallAndUsesLaunchd(t *testing.T) {
	out, err := Build("192.168.10.11:7420", "macos", unixInfo())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{
		"#!/usr/bin/env bash", `case "$(uname -m)" in`,
		`arm64) url="https://192.168.10.11:7420/agent-binaries/darwin/arm64"; want="aaaa" ;;`,
		`amd64) url="https://192.168.10.11:7420/agent-binaries/darwin/amd64"; want="bbbb" ;;`,
		"curl -fsS -k ", "shasum -a 256", "LaunchAgents/com.homeharness.agent.plist", "launchctl bootstrap",
		"<key>KeepAlive</key><true/>", `"$dir/agent" -pairing-token secret-token -manager-fingerprint abc123`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected macOS script to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "-manager-addr") {
		t.Error("LAN install must rediscover the manager rather than pin its IP")
	}
	if strings.Index(out, `"$got" != "$want"`) > strings.Index(out, `mv -f "$dir/agent.new" "$dir/agent"`) {
		t.Error("hash verification must precede installation")
	}
}

func TestBuildLinuxUsesSystemdWithFallback(t *testing.T) {
	info := unixInfo()
	info.ArchBuilds = map[string]ArchBuild{"amd64": {SHA256: "cccc", Path: "/agent-binaries/linux/amd64"}}
	out, err := Build("192.168.10.11:7420", "linux", info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"systemctl --user enable", "Restart=always", "command -v crontab", "nohup /bin/bash", `amd64) url=`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected Linux script to contain %q", want)
		}
	}
	if strings.Contains(out, "arm64) url=") {
		t.Error("only loaded architectures may appear")
	}
}

func TestBuildUnixRemoteAndErrors(t *testing.T) {
	info := unixInfo()
	info.RelayAvailable, info.RelayAddr, info.RelayToken = true, "relay.example.com:8420", "ha1.cred"
	info.BootstrapURL = "https://relay.example.com:8443/enroll/tok/agent-binary"
	out, err := BuildMode(ModeRemote, "", "macos", info)
	if err != nil {
		t.Fatalf("BuildMode remote: %v", err)
	}
	if !strings.Contains(out, `url="https://relay.example.com:8443/enroll/tok/agent-binary/arm64"`) || !strings.Contains(out, "-relay-token ha1.cred") {
		t.Errorf("remote macOS script should fetch per-arch from the bootstrap URL with relay flags:\n%s", out)
	}
	info.BootstrapURL = ""
	if out, _ := BuildMode(ModeRemote, "", "linux", info); !strings.Contains(out, "cp ./agent") {
		t.Error("manual relay path should install ./agent")
	}
	noBuilds := unixInfo()
	noBuilds.ArchBuilds = nil
	if _, err := Build("192.168.10.11:7420", "macos", noBuilds); err == nil {
		t.Error("expected an error with no macOS builds")
	}
	if _, err := Build("192.168.10.11:7420", "beos", unixInfo()); err == nil || !strings.Contains(err.Error(), "macos") {
		t.Errorf("unknown platform error should list the platforms, got %v", err)
	}
}

// The agent must never be installed to run as SYSTEM (raw commands would
// run with full control of the machine).
func TestWindowsInstallNeverRunsAsSystem(t *testing.T) {
	out := windowsInstall("fetch", "HASH", "-flags", "done")
	for _, bad := range []string{"-UserId SYSTEM", "-UserId \"SYSTEM\"", "S-1-5-18", "-RunLevel Highest", "LocalSystem", "NT AUTHORITY"} {
		if strings.Contains(out, bad) {
			t.Fatalf("the Windows install mentions %q", bad)
		}
	}
}
