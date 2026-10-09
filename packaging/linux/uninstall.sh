#!/bin/sh
# Removes what install.sh (or the .deb/.rpm package) put on a Linux machine.
#
#   sudo sh uninstall.sh            stop and remove the services and programs; KEEP configuration and data
#   sudo sh uninstall.sh --purge    also delete configuration, data and the service users
#
# --purge deletes the manager's state (database, operator token, certificate,
# stored files) and the agent's identity. That is permanent: devices that
# trusted this manager will not trust a new one. To keep a copy first:
#   sudo systemctl stop home-harness-manager
#   home-harness-manager -export-state manager-state.zip -state-dir /var/lib/home-harness/manager
#
# For testing, HH_ROOT=/some/dir works on that folder and touches no users or services.
set -eu

root="${HH_ROOT:-}"
purge=0
for a in "$@"; do
  case "$a" in
    --purge) purge=1 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "uninstall.sh: unknown option $a (try --help)" >&2; exit 2 ;;
  esac
done
if [ -z "$root" ] && [ "$(id -u)" -ne 0 ]; then
  echo "uninstall.sh: run it as root (sudo sh uninstall.sh)." >&2
  exit 1
fi

if [ -z "$root" ] && command -v systemctl >/dev/null 2>&1; then
  for u in home-harness-agent home-harness-manager; do
    systemctl disable --now "$u.service" 2>/dev/null || true
  done
fi

rm -f "$root/usr/lib/systemd/system/home-harness-manager.service" "$root/usr/lib/systemd/system/home-harness-agent.service" \
      "$root/etc/systemd/system/home-harness-manager.service" "$root/etc/systemd/system/home-harness-agent.service" \
      "$root/usr/bin/home-harness-manager" "$root/usr/bin/home-harness-agent" "$root/usr/bin/harnessctl" \
      "$root/usr/libexec/home-harness/run-manager"
rmdir "$root/usr/libexec/home-harness" 2>/dev/null || true
rm -rf "$root/usr/share/home-harness"
if [ -z "$root" ] && command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi
echo "Removed the services and programs."

if [ "$purge" = 1 ]; then
  rm -rf "$root/etc/home-harness" "$root/var/lib/home-harness"
  if [ -z "$root" ]; then
    userdel homeharness 2>/dev/null || true
    userdel homeharness-agent 2>/dev/null || true
  fi
  echo "Deleted configuration, data and the service users."
else
  echo "Kept /etc/home-harness and /var/lib/home-harness (run again with --purge to delete them)."
fi

# The per-user agent the dashboard's join script installs is separate; remove it with:
#   systemctl --user disable --now home-harness-agent; rm -rf ~/.local/share/home-harness ~/.config/systemd/user/home-harness-agent.service
echo "If you also joined this machine with the dashboard's one-line script, see docs/uninstall.md for removing that per-user agent."
