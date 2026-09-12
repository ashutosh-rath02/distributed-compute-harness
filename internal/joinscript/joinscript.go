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
}

// Build returns the onboarding script for platform ("windows" or
// "android") targeting the manager at addr (host:port, e.g.
// "192.168.10.11:7420" — the manager's own -addr value, not auto-detected;
// see the package doc and v5's plan for why this stays an explicit,
// operator-supplied fact rather than a guess). Returns an error for the
// same cases the CLI has always rejected: a malformed address, an unknown
// platform, or a manager with no agent binary configured.
func Build(addr, platform string, info Info) (string, error) {
	// net.SplitHostPort(":7420") returns host="", nil error — a legal
	// listen-address form, but useless here: it's also the manager's own
	// -addr default and startup log text, so an operator copying that
	// literally would otherwise sail past this check and generate a
	// download URL pointing at nothing ("https://:7420/agent-binary").
	if host, _, err := net.SplitHostPort(addr); err != nil || host == "" {
		return "", fmt.Errorf("invalid manager address %q (want host:port, e.g. 192.168.10.11:7420)", addr)
	}
	if platform != "windows" && platform != "android" {
		return "", fmt.Errorf("unknown platform %q (want %q or %q)", platform, "windows", "android")
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

	if platform == "android" {
		return fmt.Sprintf(`One-time prerequisites on the phone, before pasting anything below:
  1. Install Termux from F-Droid (not the Play Store build — it's deprecated/frozen): https://f-droid.org/packages/com.termux/
  2. Install Termux:Boot from F-Droid too: https://f-droid.org/packages/com.termux.boot/
  3. Open Termux once, then in Android's battery settings, disable battery
     optimization for Termux (best-effort against OEM background killers —
     no software fix eliminates this entirely on every phone).

Then paste this into Termux:

pkg install -y curl && mkdir -p ~/home-harness && cd ~/home-harness && curl %s-o agent "%s://%s/agent-binary" && [ "$(sha256sum agent | awk '{print $1}')" = "%s" ] && chmod +x agent && mkdir -p ~/.termux/boot && printf '#!/data/data/com.termux/files/usr/bin/bash\n/data/data/com.termux/files/usr/bin/termux-wake-lock\ncd ~/home-harness\nwhile true; do ./agent -manager-addr %s -pairing-token %s %s; sleep 5; done\n' > ~/.termux/boot/start-harness-agent.sh && chmod +x ~/.termux/boot/start-harness-agent.sh && (nohup ~/.termux/boot/start-harness-agent.sh >~/home-harness/agent.log 2>&1 &) && echo "Installed — agent running, and will auto-start on reboot via Termux:Boot."
`, curlFlag, scheme, addr, strings.ToLower(hash), addr, info.PairingToken, authFlag), nil
	}

	return fmt.Sprintf(`Paste this into a PowerShell terminal on the new machine:

curl.exe %s"%s://%s/agent-binary" -o agent.exe
if ((Get-FileHash agent.exe -Algorithm SHA256).Hash -ne "%s") { throw "agent.exe hash mismatch — download corrupted or tampered with, aborting" } else { .\agent.exe -manager-addr %s -pairing-token %s %s }
`, curlFlag, scheme, addr, hash, addr, info.PairingToken, authFlag), nil
}
