// Package joinscript builds the copy-pasteable onboarding script for a new
// LAN machine (v5 part 1/1b) — the exact text cmd/harnessctl's `join`
// command prints, and (v5 part 2) what the manager's own web dashboard
// serves via GET /join-script. Extracted into its own package so there is
// exactly one place that knows the script format: the CLI and the
// dashboard both call Build rather than keeping two copies that could
// drift apart.
package joinscript

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Info is what a new node needs to onboard itself — mirrors
// manager.JoinInfo's fields (internal/manager/join.go), duplicated here
// rather than imported so this package stays free of an internal/manager
// dependency (cmd/harnessctl already avoids that import today).
type Info struct {
	Fingerprint          string
	PairingToken         string
	Insecure             bool
	AgentBinaryAvailable bool
	AgentBinarySHA256    string
	RelayAvailable       bool
	RelayAddr            string
	RelayToken           string
	AgentBinaryPath      string
	BootstrapURL         string
	// ArchBuilds is used by the macOS/Linux scripts instead of the single
	// AgentBinarySHA256/Path: every build for that OS keyed by GOARCH, so
	// the script can pick its own CPU's at install time.
	ArchBuilds map[string]ArchBuild
}

// TargetPlatform maps an onboarding platform name to the GOOS/GOARCH of
// the agent build it installs — the one place that mapping lives, so the
// manager's catalog lookup, invitations, and harnessctl agree. Android
// agents run under Termux as ordinary GOOS=linux binaries.
func TargetPlatform(platform string) (goos, arch string, ok bool) {
	switch platform {
	case "windows":
		return "windows", "amd64", true
	case "android":
		return "linux", "arm64", true
	}
	return "", "", false
}

type Mode string

const (
	ModeLAN    Mode = "lan"
	ModeRemote Mode = "remote"
)

// Build returns the onboarding script for platform ("windows" or
// "android") targeting the manager at addr (host:port, e.g.
// "192.168.10.11:7420" — the manager's own -addr value, not auto-detected;
// see the package doc and v5's plan for why this stays an explicit,
// operator-supplied fact rather than a guess). Returns an error for the
// same cases the CLI has always rejected: a malformed address, an unknown
// platform, or a manager with no agent binary configured.
func Build(addr, platform string, info Info) (string, error) {
	return BuildMode(ModeLAN, addr, platform, info)
}

// BuildMode builds either the existing LAN bootstrap or a relay-connected
// bootstrap. Remote mode downloads from BootstrapURL when a public relay
// enrollment supplied one; the legacy/manual path verifies and launches a
// binary the operator placed locally.
func BuildMode(mode Mode, addr, platform string, info Info) (string, error) {
	if mode != ModeLAN && mode != ModeRemote {
		return "", fmt.Errorf("unknown connection mode %q (want %q or %q)", mode, ModeLAN, ModeRemote)
	}
	if mode == ModeRemote {
		return buildRemote(platform, info)
	}
	// net.SplitHostPort(":7420") returns host="", nil error — a legal
	// listen-address form, but useless here: it's also the manager's own
	// -addr default and startup log text, so an operator copying that
	// literally would otherwise sail past this check and generate a
	// download URL pointing at nothing ("https://:7420/agent-binary").
	if err := ValidateAddress(addr); err != nil {
		return "", fmt.Errorf("invalid manager address %q (want host:port, e.g. 192.168.10.11:7420): %w", addr, err)
	}
	if !KnownPlatform(platform) {
		return "", fmt.Errorf("unknown platform %q (want %s)", platform, Platforms)
	}
	if !info.AgentBinaryAvailable {
		return "", fmt.Errorf("manager has no agent binary configured — restart it with -agent-binary (pointed at the right build for %s) to enable joining", platform)
	}

	scheme := "https"
	curlFlag := "-k "
	authFlag := fmt.Sprintf("-manager-fingerprint %s", info.Fingerprint)
	if info.Insecure {
		scheme = "http"
		curlFlag = ""
		authFlag = "-insecure"
	}
	hash := strings.ToUpper(info.AgentBinarySHA256)
	binaryPath := info.AgentBinaryPath
	if binaryPath == "" {
		binaryPath = "/agent-binary"
	}

	if _, unix := UnixPlatformOS(platform); unix {
		return unixInstall(platform, info.ArchBuilds,
			func(_ string, b ArchBuild) string { return scheme + "://" + addr + b.Path },
			curlFlag, fmt.Sprintf("-pairing-token %s %s", info.PairingToken, authFlag),
			"Installed. The agent is running and finds the manager on your network by itself.")
	}

	if platform == "android" {
		return fmt.Sprintf(`One-time prerequisites on the phone, before pasting anything below:
  1. Install Termux from F-Droid (not the Play Store build — it's deprecated/frozen): https://f-droid.org/packages/com.termux/
  2. Install Termux:Boot from F-Droid too: https://f-droid.org/packages/com.termux.boot/
  3. Open Termux once, then in Android's battery settings, disable battery
     optimization for Termux (best-effort against OEM background killers —
     no software fix eliminates this entirely on every phone).

Then paste this into Termux:

pkg install -y curl && mkdir -p ~/home-harness && cd ~/home-harness && curl %s-o agent "%s://%s" && [ "$(sha256sum agent | awk '{print $1}')" = "%s" ] && chmod +x agent && mkdir -p ~/.termux/boot && printf '#!/data/data/com.termux/files/usr/bin/bash\n/data/data/com.termux/files/usr/bin/termux-wake-lock\nexport HOME_HARNESS_SUPERVISED=1\ncd ~/home-harness\nwhile true; do ./agent -pairing-token %s %s; sleep 5; done\n' > ~/.termux/boot/start-harness-agent.sh && chmod +x ~/.termux/boot/start-harness-agent.sh && (nohup ~/.termux/boot/start-harness-agent.sh >~/home-harness/agent.log 2>&1 &) && echo "Installed — agent running with LAN discovery, and will auto-start on reboot via Termux:Boot."
`, curlFlag, scheme, addr+binaryPath, strings.ToLower(hash), info.PairingToken, authFlag), nil
	}

	return "Paste this into PowerShell. It installs the agent for the current user and reconnects automatically at every logon using LAN discovery:\n\n" +
		windowsInstall(
			fmt.Sprintf(`curl.exe %s"%s://%s" -o $download`, curlFlag, scheme, addr+binaryPath),
			hash,
			fmt.Sprintf("-pairing-token %s %s", info.PairingToken, authFlag),
			"Installed. The agent now discovers the phone on the LAN and reconnects automatically after logon.",
		), nil
}

// windowsInstall is the one PowerShell installation block every Windows
// onboarding path shares: fetch places the candidate binary at $download,
// which must match hash before it replaces the installed agent; then a
// per-user HKCU Run launcher keeps the agent (run with agentFlags) alive
// across crashes and logons. Sharing it keeps LAN and relay onboarding
// from drifting apart on persistence — a remote node that silently stops
// at window close is exactly the kind of regression that would otherwise
// only show up on real hardware.
func windowsInstall(fetch, hash, agentFlags, done string) string {
	return fmt.Sprintf(`$root = Join-Path $env:LOCALAPPDATA "HomeHarness"
New-Item -ItemType Directory -Force $root | Out-Null
$download = Join-Path $root "agent.new.exe"
$agent = Join-Path $root "agent.exe"
%s
if ((Get-FileHash $download -Algorithm SHA256).Hash -ne "%s") { Remove-Item $download -Force; throw "agent.exe hash mismatch — wrong, corrupted, or tampered binary; aborting" }
Move-Item $download $agent -Force
$launcher = Join-Path $root "start-agent.ps1"
@'
$agent = Join-Path $env:LOCALAPPDATA "HomeHarness\agent.exe"
# Tells the agent this loop restarts it, so a self-update just exits and
# lets the loop start the new binary instead of running a second copy.
$env:HOME_HARNESS_SUPERVISED = "1"
while ($true) {
  & $agent %s
  Start-Sleep -Seconds 5
}
'@ | Set-Content -Encoding UTF8 $launcher
# Start at sign-in as a per-user scheduled task: hidden, restarted if
# it ever stops, kept running on battery, and never elevated (no admin
# needed; raw commands run as you, not SYSTEM). The older Run-key entry
# is the fallback, and is removed when the task takes over so only one
# launcher ever loops.
$taskName = "HomeComputeHarnessAgent"
$runKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
$launchArgs = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $launcher + '"'
$task = $false
try {
  $user = "$env:USERDOMAIN\$env:USERNAME"
  $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument $launchArgs
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
  $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
  Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description "Home Compute Harness agent" -Force -ErrorAction Stop | Out-Null
  Remove-ItemProperty -Path $runKey -Name $taskName -ErrorAction SilentlyContinue
  $task = $true
} catch {
  Write-Warning ("Couldn't register a scheduled task (" + $_.Exception.Message + "); starting at sign-in from the Run key instead.")
  New-ItemProperty -Path $runKey -Name $taskName -Value ("powershell.exe " + $launchArgs) -PropertyType String -Force | Out-Null
}
if ($task) { Start-ScheduledTask -TaskName $taskName } else { Start-Process powershell.exe -ArgumentList $launchArgs -WindowStyle Hidden }
Write-Host "%s"
`, fetch, hash, agentFlags, done)
}

// ValidateAddress accepts an IP address or conservative DNS hostname plus
// a numeric TCP port. Besides producing usable URLs, this keeps an
// operator-entered address from becoming shell syntax in generated scripts.
func ValidateAddress(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("missing host or port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if len(host) > 253 {
		return fmt.Errorf("hostname is too long")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid hostname")
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
				return fmt.Errorf("invalid hostname")
			}
		}
	}
	return nil
}

func buildRemote(platform string, info Info) (string, error) {
	if !KnownPlatform(platform) {
		return "", fmt.Errorf("unknown platform %q (want %s)", platform, Platforms)
	}
	if !info.RelayAvailable || info.RelayAddr == "" || info.RelayToken == "" {
		return "", fmt.Errorf("manager has no relay configured — restart it with -relay-addr and -relay-token to enable remote joining")
	}
	if err := ValidateAddress(info.RelayAddr); err != nil {
		return "", fmt.Errorf("invalid relay address %q (want host:port, e.g. relay.example.com:8420)", info.RelayAddr)
	}
	if !info.AgentBinaryAvailable {
		return "", fmt.Errorf("manager has no agent binary configured — restart it with -agent-binary (pointed at the right build for %s) to enable joining", platform)
	}

	authFlag := fmt.Sprintf("-manager-fingerprint %s", info.Fingerprint)
	if info.Insecure {
		authFlag = "-insecure"
	}
	hash := strings.ToUpper(info.AgentBinarySHA256)
	flags := fmt.Sprintf("-pairing-token %s %s -relay-addr %s -relay-token %s", info.PairingToken, authFlag, info.RelayAddr, info.RelayToken)
	if _, unix := UnixPlatformOS(platform); unix {
		urlFor := func(string, ArchBuild) string { return "" } // manual: ./agent
		if info.BootstrapURL != "" {
			urlFor = func(arch string, _ ArchBuild) string { return info.BootstrapURL + "/" + arch }
		}
		return unixInstall(platform, info.ArchBuilds, urlFor, "-L ", flags,
			"Installed. The agent is running and connects through the relay.")
	}
	if info.BootstrapURL != "" {
		if platform == "android" {
			return fmt.Sprintf(`One-time prerequisites on the phone:
  1. Install Termux and Termux:Boot from F-Droid, then open Termux once.
  2. Disable Android battery optimization for Termux where available.

Then paste this into Termux:

pkg install -y curl && mkdir -p ~/home-harness && cd ~/home-harness && curl -fLo agent "%s" && [ "$(sha256sum agent | awk '{print $1}')" = "%s" ] && chmod +x agent && mkdir -p ~/.termux/boot && printf '#!/data/data/com.termux/files/usr/bin/bash\n/data/data/com.termux/files/usr/bin/termux-wake-lock\nexport HOME_HARNESS_SUPERVISED=1\ncd ~/home-harness\nwhile true; do ./agent %s; sleep 5; done\n' > ~/.termux/boot/start-harness-agent.sh && chmod +x ~/.termux/boot/start-harness-agent.sh && (nohup ~/.termux/boot/start-harness-agent.sh >~/home-harness/agent.log 2>&1 &) && echo "Installed — agent connected through the relay."
`, info.BootstrapURL, strings.ToLower(hash), flags), nil
		}
		return "Paste this into PowerShell. It installs the agent for the current user and reconnects through the relay automatically at every logon:\n\n" +
			windowsInstall(
				fmt.Sprintf(`curl.exe -fL "%s" -o $download`, info.BootstrapURL),
				hash, flags,
				"Installed. The agent now connects through the relay and reconnects automatically after logon.",
			), nil
	}

	if platform == "android" {
		return fmt.Sprintf(`One-time prerequisites on the phone:
  1. Install Termux and Termux:Boot from F-Droid, then open Termux once.
  2. Disable Android battery optimization for Termux where available.
  3. Place the Android/Termux agent binary at ~/home-harness/agent.

This manual path has no download step; use a QR/link invitation from a manager
with -relay-public-url configured to have the binary fetched for you.

Then paste this into Termux:

mkdir -p ~/home-harness && cd ~/home-harness && [ "$(sha256sum agent | awk '{print $1}')" = "%s" ] && chmod +x agent && mkdir -p ~/.termux/boot && printf '#!/data/data/com.termux/files/usr/bin/bash\n/data/data/com.termux/files/usr/bin/termux-wake-lock\nexport HOME_HARNESS_SUPERVISED=1\ncd ~/home-harness\nwhile true; do ./agent %s; sleep 5; done\n' > ~/.termux/boot/start-harness-agent.sh && chmod +x ~/.termux/boot/start-harness-agent.sh && (nohup ~/.termux/boot/start-harness-agent.sh >~/home-harness/agent.log 2>&1 &) && echo "Installed — agent running through the relay, and will auto-start on reboot via Termux:Boot."
`, strings.ToLower(hash), flags), nil
	}

	return `Before running this, place the Windows agent binary as agent.exe in the current directory.
This manual path has no download step; use a QR/link invitation from a manager
with -relay-public-url configured to have the binary fetched for you.

Then paste this into PowerShell. It installs the agent for the current user and reconnects through the relay automatically at every logon:

` + windowsInstall(
		`Copy-Item -LiteralPath (Join-Path (Get-Location) "agent.exe") $download -Force`,
		hash, flags,
		"Installed. The agent now connects through the relay and reconnects automatically after logon.",
	), nil
}
