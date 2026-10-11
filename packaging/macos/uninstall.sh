#!/bin/bash
# Removes Home Harness from this Mac: the package's files, and the per-user
# agent the dashboard's one-line join script installs.
#
#   bash uninstall.sh            remove the programs and services; KEEP your data
#   bash uninstall.sh --purge    also delete ~/Library/Application Support/HomeHarness
#                                (the manager's database, tokens and certificate, and the agent's identity)
#
# --purge is permanent: devices that trusted this manager will not trust a new
# one. Run it as yourself (not with sudo); it asks for sudo only for the
# package files in /usr/local and /Library.
#
# HH_ROOT=/some/dir works on that folder instead of / and touches no services (for testing).
set -u

purge=0
for a in "$@"; do
  case "$a" in
    --purge) purge=1 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "uninstall.sh: unknown option $a (try --help)" >&2; exit 2 ;;
  esac
done

root="${HH_ROOT:-}"
home="${HH_HOME:-$HOME}"
support="$home/Library/Application Support/HomeHarness"
as_root() { if [ -n "$root" ] || [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }

if [ -z "$root" ]; then
  uid="$(id -u)"
  for label in com.homeharness.manager com.homeharness.agent; do
    launchctl bootout "gui/$uid/$label" 2>/dev/null || true
  done
fi

# The join script's per-user agent, and any copy of the plists in the user's folder.
rm -f "$home/Library/LaunchAgents/com.homeharness.agent.plist" "$home/Library/LaunchAgents/com.homeharness.manager.plist"

# The package's files.
as_root rm -f "$root/Library/LaunchAgents/com.homeharness.manager.plist" "$root/Library/LaunchAgents/com.homeharness.agent.plist" \
  "$root/usr/local/bin/harnessctl"
as_root rm -rf "$root/usr/local/libexec/home-harness" "$root/usr/local/share/home-harness"
if [ -z "$root" ] && command -v pkgutil >/dev/null 2>&1; then
  as_root pkgutil --forget com.homeharness.pkg >/dev/null 2>&1 || true
fi
echo "Removed the programs and services."

if [ "$purge" = 1 ]; then
  rm -rf "$support"
  echo "Deleted $support."
else
  # Programs the join script put in the support folder go; the data stays.
  rm -f "$support/agent" "$support/agent.new" "$support/run-agent.sh" "$support/agent.log"
  echo "Kept your data in $support (run again with --purge to delete it)."
fi
