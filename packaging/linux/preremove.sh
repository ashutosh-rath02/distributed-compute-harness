#!/bin/sh
# Package pre-remove: stop the services on a real removal (not on an upgrade).
# Deb passes "remove"/"upgrade"; rpm passes 0 (removal) or 1 (upgrade).
set -e
case "${1:-remove}" in
  upgrade|1) exit 0 ;;
esac
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl disable --now home-harness-agent.service 2>/dev/null || true
  systemctl disable --now home-harness-manager.service 2>/dev/null || true
fi
