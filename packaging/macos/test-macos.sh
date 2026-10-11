#!/bin/bash
# What can be tested without a Mac: script syntax, plist well-formedness (when
# python3 is there), the launch wrappers against fake programs, and the
# uninstaller against a throwaway folder. pkgbuild/productbuild and launchd
# themselves need a Mac and are NOT exercised here.
#   bash packaging/macos/test-macos.sh
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
check() { if eval "$2"; then echo "ok    $1"; else echo "FAIL  $1"; fail=1; fi; }

for f in "$here"/*.sh "$here/postinstall"; do bash -n "$f" || { echo "FAIL  syntax $f"; fail=1; }; done
check "scripts parse" '[ "$fail" = 0 ]'

py=""
for cand in python3 python; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import plistlib' >/dev/null 2>&1; then py="$cand"; break; fi
done
if [ -n "$py" ]; then
  for p in "$here"/*.plist; do
    check "$(basename "$p") is valid XML with a Label" "\"$py\" -c 'import sys,plistlib;d=plistlib.load(open(sys.argv[1],\"rb\"));assert d[\"Label\"] and d[\"ProgramArguments\"]' \"$p\""
  done
fi
check "the agent plist uses the join script's label" 'grep -q "<string>com.homeharness.agent</string>" "$here/com.homeharness.agent.plist"'

# Wrappers with fake programs.
mkdir -p "$tmp/lib" "$tmp/prefix/share/home-harness/agents" "$tmp/support"
printf '#!/bin/sh\necho "$@"\n' > "$tmp/lib/manager"; cp "$tmp/lib/manager" "$tmp/lib/agent"; chmod +x "$tmp/lib/manager" "$tmp/lib/agent"
echo x > "$tmp/prefix/share/home-harness/agents/agent-linux-arm64"
export HH_SUPPORT_DIR="$tmp/support" HH_LIBEXEC="$tmp/lib" HH_PREFIX="$tmp/prefix"
bash "$here/run-manager.sh"
log="$(cat "$tmp/support/manager/manager.log")"
check "manager wrapper creates a pairing token" '[ "$(wc -c < "$tmp/support/manager/pairing-token" | tr -d " ")" = 64 ]'
check "manager wrapper passes it as a flag" 'echo "$log" | grep -q -- "-pairing-token $(cat "$tmp/support/manager/pairing-token")"'
check "manager wrapper serves the other platforms' agents" 'echo "$log" | grep -q -- "-agent-binary .*agent-linux-arm64"'
echo "-pair -manager-addr h:7420" > "$tmp/support/agent.args"
bash "$here/run-agent.sh"
check "agent wrapper passes agent.args" 'grep -q -- "-pair -manager-addr h:7420" "$tmp/support/agent.log"'
check "agent wrapper disables self-update (the package manager updates it)" 'grep -q -- "-no-self-update" "$tmp/support/agent.log"'

# Uninstaller against a throwaway root and home.
r="$tmp/root"; h="$tmp/home"
mkdir -p "$r/usr/local/libexec/home-harness" "$r/usr/local/share/home-harness" "$r/usr/local/bin" "$r/Library/LaunchAgents" "$h/Library/LaunchAgents" "$h/Library/Application Support/HomeHarness/manager"
touch "$r/usr/local/bin/harnessctl" "$r/Library/LaunchAgents/com.homeharness.manager.plist" "$h/Library/LaunchAgents/com.homeharness.agent.plist" "$h/Library/Application Support/HomeHarness/manager/operator-token" "$h/Library/Application Support/HomeHarness/agent"
HH_ROOT="$r" HH_HOME="$h" bash "$here/uninstall.sh" >/dev/null
check "uninstall removes the package files" '[ ! -e "$r/usr/local/libexec/home-harness" ] && [ ! -e "$r/usr/local/bin/harnessctl" ] && [ ! -e "$r/Library/LaunchAgents/com.homeharness.manager.plist" ]'
check "uninstall removes the join script's launch agent" '[ ! -e "$h/Library/LaunchAgents/com.homeharness.agent.plist" ]'
check "uninstall keeps data without --purge" '[ -f "$h/Library/Application Support/HomeHarness/manager/operator-token" ]'
HH_ROOT="$r" HH_HOME="$h" bash "$here/uninstall.sh" --purge >/dev/null
check "--purge deletes the data" '[ ! -e "$h/Library/Application Support/HomeHarness" ]'

[ "$fail" = 0 ] && echo "all checks passed"
exit "$fail"
