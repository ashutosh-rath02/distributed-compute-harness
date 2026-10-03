package joinscript

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// PairingInfo is what a join-page installer needs. It holds no secret:
// the device is admitted only once the operator approves it on the
// manager, after comparing the pairing code the installer prints with the
// one the manager shows.
type PairingInfo struct {
	// Fingerprint is the manager's TLS certificate fingerprint, pinned by
	// the agent (and part of the pairing code).
	Fingerprint string
	// FallbackAddr is the manager's agent address as this device reached
	// the join page (host:port), for when LAN discovery finds nothing.
	FallbackAddr string
	// WindowsURL and WindowsSHA256 name the windows/amd64 build.
	WindowsURL, WindowsSHA256 string
	// ArchBuilds are the macOS/Linux builds by GOARCH; Path is the full
	// download URL.
	ArchBuilds map[string]ArchBuild
}

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const approveHint = "Now approve this device where you manage Home Harness (the app, or the dashboard). Check that the code there is the same."

// BuildPairing renders the installer the join page hands out for
// platform ("windows", "macos" or "linux"): it installs the agent in
// pairing mode, creates its identity, starts it, and prints the pairing
// code. The result is a script with no surrounding prose, run as
// `irm <url> | iex` (Windows) or `curl -fsS <url> | bash`.
func BuildPairing(platform string, p PairingInfo) (string, error) {
	if !fingerprintPattern.MatchString(p.Fingerprint) {
		return "", errors.New("joining by approval needs a manager with TLS (no -insecure)")
	}
	if err := ValidateAddress(p.FallbackAddr); err != nil {
		return "", fmt.Errorf("invalid manager address %q: %w", p.FallbackAddr, err)
	}
	flags := fmt.Sprintf("-pair -manager-fingerprint %s -manager-addr-fallback %s", p.Fingerprint, p.FallbackAddr)
	codeArgs := "-pair-code -manager-fingerprint " + p.Fingerprint
	if _, unix := UnixPlatformOS(platform); unix {
		return unixInstallSteps(platform, p.ArchBuilds, func(_ string, b ArchBuild) string { return b.Path }, "", flags,
			`"$dir/agent" `+codeArgs+` >/dev/null`,
			`echo; "$dir/agent" `+codeArgs+`; echo "`+approveHint+`"`,
			"The agent keeps running and starts by itself when you log in.")
	}
	if platform != "windows" {
		return "", fmt.Errorf("no join-page installer for %q (Android devices use the app)", platform)
	}
	if p.WindowsURL == "" || p.WindowsSHA256 == "" {
		return "", errors.New("manager has no Windows agent build loaded")
	}
	return windowsInstall(windowsSteps{
		Fetch:       fmt.Sprintf(`curl.exe -fsS "%s" -o $download`, p.WindowsURL),
		Hash:        strings.ToUpper(p.WindowsSHA256),
		AgentFlags:  flags,
		BeforeStart: "& $agent " + codeArgs + " | Out-Null\nif ($LASTEXITCODE -ne 0) { throw \"the agent could not create its identity\" }",
		AfterStart:  "Write-Host \"\"\n& $agent " + codeArgs + "\nWrite-Host \"" + approveHint + "\"",
		Done:        "The agent keeps running and starts by itself when you sign in.",
	}), nil
}
