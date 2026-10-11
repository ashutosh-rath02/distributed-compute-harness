#!/bin/sh
# Package post-remove. Removing the package keeps your data and configuration
# (/var/lib/home-harness, /etc/home-harness); `apt purge` also removes the
# configuration files it owns. The data, which includes the manager's identity,
# is deleted only by you: sudo rm -rf /var/lib/home-harness
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
fi
