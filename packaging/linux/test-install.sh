#!/bin/sh
# Tests install.sh and uninstall.sh against a throwaway folder (HH_ROOT), with
# fake programs. Touches no users, services or real paths. Runs anywhere with sh.
#   sh packaging/linux/test-install.sh
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
check() { if eval "$2"; then echo "ok    $1"; else echo "FAIL  $1"; fail=1; fi; }

for f in "$here"/*.sh; do sh -n "$f" || { echo "FAIL  syntax $f"; fail=1; }; done

mkdir -p "$tmp/bin/agents"
for n in manager agent harnessctl; do echo x > "$tmp/bin/$n"; done
echo x > "$tmp/bin/agents/agent-linux-arm64"
export HH_ROOT="$tmp/root"

sh "$here/install.sh" --bin-dir "$tmp/bin" --agents-dir "$tmp/bin/agents" >/dev/null
check "manager installed" '[ -f "$HH_ROOT/usr/bin/home-harness-manager" ]'
check "agent installed" '[ -f "$HH_ROOT/usr/bin/home-harness-agent" ]'
check "wrapper installed" '[ -f "$HH_ROOT/usr/libexec/home-harness/run-manager" ]'
check "units installed" '[ -f "$HH_ROOT/usr/lib/systemd/system/home-harness-manager.service" ] && [ -f "$HH_ROOT/usr/lib/systemd/system/home-harness-agent.service" ]'
check "other platforms agent builds installed" '[ -f "$HH_ROOT/usr/share/home-harness/agents/agent-linux-arm64" ]'
check "pairing token is 64 hex characters" '[ "$(wc -c < "$HH_ROOT/etc/home-harness/pairing-token" | tr -d " ")" = 64 ]'
tok="$(cat "$HH_ROOT/etc/home-harness/pairing-token")"
echo "MANAGER_ARGS=\"-advertise-addr 10.0.0.1:7420\"" > "$HH_ROOT/etc/home-harness/manager.env"

sh "$here/install.sh" --bin-dir "$tmp/bin" >/dev/null
check "re-install keeps the pairing token" '[ "$(cat "$HH_ROOT/etc/home-harness/pairing-token")" = "$tok" ]'
check "re-install keeps edited configuration" 'grep -q 10.0.0.1 "$HH_ROOT/etc/home-harness/manager.env"'

# The wrapper builds the right command line (with a fake manager that prints it).
printf '#!/bin/sh\necho "$@"\n' > "$tmp/fake-manager"; chmod +x "$tmp/fake-manager"
sed "s#/usr/bin/home-harness-manager#$tmp/fake-manager#" "$here/run-manager.sh" > "$tmp/run"
out="$(HH_CONFIG_DIR="$HH_ROOT/etc/home-harness" HH_SHARE_DIR="$HH_ROOT/usr/share/home-harness" STATE_DIRECTORY="$tmp/state" MANAGER_ARGS="-x" sh "$tmp/run")"
check "wrapper passes the token from the file" 'echo "$out" | grep -q -- "-pairing-token $tok"'
check "wrapper adds the agent builds" 'echo "$out" | grep -q -- "-agent-binary .*agent-linux-arm64"'
check "wrapper passes MANAGER_ARGS" 'echo "$out" | grep -q -- " -x"'

sh "$here/uninstall.sh" >/dev/null
check "uninstall removes the programs" '[ ! -e "$HH_ROOT/usr/bin/home-harness-manager" ] && [ ! -e "$HH_ROOT/usr/bin/harnessctl" ]'
check "uninstall removes the units" '[ ! -e "$HH_ROOT/usr/lib/systemd/system/home-harness-manager.service" ]'
check "uninstall keeps configuration without --purge" '[ -f "$HH_ROOT/etc/home-harness/pairing-token" ]'
sh "$here/uninstall.sh" --purge >/dev/null
check "--purge removes configuration" '[ ! -e "$HH_ROOT/etc/home-harness" ]'

# Hardening keys every unit must keep.
for u in manager agent; do
  for k in NoNewPrivileges=true ProtectSystem=strict PrivateTmp=true; do
    check "$u unit has $k" 'grep -q "^$k" "$here/home-harness-$u.service"'
  done
  check "$u unit has a state directory" 'grep -q "^StateDirectory=" "$here/home-harness-$u.service"'
done
[ "$fail" = 0 ] && echo "all checks passed"
exit "$fail"
