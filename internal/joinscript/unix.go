package joinscript

import (
	"fmt"
	"sort"
	"strings"
)

// unixPlatforms maps desktop-Unix onboarding names to GOOS. Unlike Windows
// and Android, a Unix machine's CPU isn't implied by its platform (Intel or
// Apple Silicon Macs, x86 or ARM Linux), so these scripts are handed every
// build the manager has for that OS and pick their own at install time.
var unixPlatforms = map[string]string{"macos": "darwin", "linux": "linux"}

// UnixPlatformOS reports the GOOS a desktop-Unix onboarding platform
// installs, if platform is one.
func UnixPlatformOS(platform string) (goos string, ok bool) {
	goos, ok = unixPlatforms[platform]
	return goos, ok
}

// KnownPlatform reports whether platform is an onboarding platform name.
func KnownPlatform(platform string) bool {
	if _, _, ok := TargetPlatform(platform); ok {
		return true
	}
	_, ok := unixPlatforms[platform]
	return ok
}

// Platforms lists every onboarding platform name, for error messages.
const Platforms = `"windows", "android", "macos", or "linux"`

// ArchBuild is one CPU architecture's agent build for a Unix script: its
// SHA-256 and the path (on the manager, or an enrollment's base) it is
// downloaded from.
type ArchBuild struct {
	SHA256 string
	Path   string
}

// unixInstall renders the bash installer for macOS or Linux. urlFor gives
// each architecture's download URL ("" means: copy ./agent from the
// current directory instead — the manual relay path). The script is valid
// bash end to end, so it can be piped (`curl … | bash`) as well as pasted,
// and every human-facing line is a comment or an echo.
func unixInstall(platform string, builds map[string]ArchBuild, urlFor func(arch string, b ArchBuild) string, curlFlags, agentFlags, done string) (string, error) {
	goos := unixPlatforms[platform]
	if len(builds) == 0 {
		return "", fmt.Errorf("manager has no %s agent build configured — restart it with -agent-binary for %s (see scripts/build-agents.sh) to enable joining", platform, goos)
	}
	arches := make([]string, 0, len(builds))
	for arch := range builds {
		arches = append(arches, arch)
	}
	sort.Strings(arches)

	var choose strings.Builder
	for _, arch := range arches {
		b := builds[arch]
		src := urlFor(arch, b)
		fmt.Fprintf(&choose, "  %s) url=%q; want=%q ;;\n", arch, src, strings.ToLower(b.SHA256))
	}

	fetch := fmt.Sprintf(`curl -fsS %s-o "$dir/agent.new" "$url"`, curlFlags)
	if urlFor(arches[0], builds[arches[0]]) == "" {
		fetch = `[ -f ./agent ] || { echo "Place the agent binary for this machine as ./agent in the current directory first." >&2; exit 1; }
cp ./agent "$dir/agent.new"`
	}

	var dir, persist string
	switch platform {
	case "macos":
		dir = `$HOME/Library/Application Support/HomeHarness`
		persist = `plist="$HOME/Library/LaunchAgents/com.homeharness.agent.plist"
mkdir -p "$HOME/Library/LaunchAgents"
cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.homeharness.agent</string>
  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$dir/run-agent.sh</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
PLIST
launchctl bootout "gui/$(id -u)/com.homeharness.agent" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist" 2>/dev/null || launchctl load -w "$plist"
echo "If macOS asks whether \"agent\" may accept incoming network connections, choose Allow: that is how it finds the manager on your network."`
	default: // linux
		dir = `$HOME/.local/share/home-harness`
		persist = `if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  unit="$HOME/.config/systemd/user/home-harness-agent.service"
  mkdir -p "$(dirname "$unit")"
  cat > "$unit" <<UNIT
[Unit]
Description=Home Compute Harness agent
After=network-online.target

[Service]
ExecStart=/bin/bash "$dir/run-agent.sh"
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable home-harness-agent.service >/dev/null
  systemctl --user restart home-harness-agent.service
  echo "To keep it running when you're logged out: sudo loginctl enable-linger ${USER:-$(id -un)}"
else
  pkill -f "home-harness/run-agent.sh" 2>/dev/null || true
  if command -v crontab >/dev/null 2>&1; then
    (crontab -l 2>/dev/null | grep -v "home-harness/run-agent.sh" || true; echo "@reboot /bin/bash \"$dir/run-agent.sh\"") | crontab - || true
  else
    echo "No systemd user session or crontab here: the agent runs now but won't start by itself after a reboot."
  fi
  nohup /bin/bash "$dir/run-agent.sh" >/dev/null 2>&1 &
fi`
	}

	return fmt.Sprintf(`#!/usr/bin/env bash
# Home Compute Harness agent installer for %[1]s.
# Run it with bash, e.g.  curl -fsSk '<this script's URL>' | bash
# (or save it to a file and run: bash install-agent.sh). It installs the
# agent for the current user and keeps it running across logins and reboots.
set -eu
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$arch" in
%[2]s  *) echo "This manager has no %[1]s agent build for $arch CPUs loaded; restart it with one (scripts/build-agents.sh)." >&2; exit 1 ;;
esac
dir="%[3]s"
mkdir -p "$dir"
%[4]s
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$dir/agent.new" | cut -d' ' -f1); else got=$(shasum -a 256 "$dir/agent.new" | cut -d' ' -f1); fi
if [ "$got" != "$want" ]; then rm -f "$dir/agent.new"; echo "Agent hash mismatch: wrong, corrupted, or tampered download; aborting." >&2; exit 1; fi
chmod 755 "$dir/agent.new"
mv -f "$dir/agent.new" "$dir/agent"
# launchd and cron start jobs with a minimal PATH (/usr/bin:/bin:...), so
# workloads needing Homebrew, /usr/local/bin, or the ollama CLI would be
# "not found" under the launcher. Freeze the installing shell's PATH in.
quoted_path=$(printf '%%s' "$PATH" | sed "s/'/'\\\\''/g")
{
  echo '#!/bin/bash'
  echo "export PATH='$quoted_path'"
  # Tells the agent a supervisor restarts it, so after a self-update it
  # just exits and lets this loop start the new binary (no second copy).
  echo 'export HOME_HARNESS_SUPERVISED=1'
  cat <<'LAUNCHER'
dir="$(cd "$(dirname "$0")" && pwd)"
while true; do
  "$dir/agent" %[5]s >> "$dir/agent.log" 2>&1
  sleep 5
done
LAUNCHER
} > "$dir/run-agent.sh"
chmod 755 "$dir/run-agent.sh"
%[6]s
echo "%[7]s"
`, platform, choose.String(), dir, fetch, agentFlags, persist, done), nil
}
