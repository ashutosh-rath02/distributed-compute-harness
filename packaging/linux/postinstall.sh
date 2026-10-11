#!/bin/sh
# Package post-install: create the service users, the pairing token and the
# data folders, and start the manager on a first install. Never overwrites
# anything that is already there.
set -e

nologin=/usr/sbin/nologin; [ -x "$nologin" ] || nologin=/sbin/nologin; [ -x "$nologin" ] || nologin=/bin/false
for u in homeharness homeharness-agent; do
  id "$u" >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell "$nologin" "$u"
done

install -d -m 0700 -o homeharness -g homeharness /var/lib/home-harness/manager
install -d -m 0700 -o homeharness-agent -g homeharness-agent /var/lib/home-harness/agent

if [ ! -f /etc/home-harness/pairing-token ]; then
  umask 077
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/home-harness/pairing-token
fi
chown root:homeharness /etc/home-harness/pairing-token
chmod 0640 /etc/home-harness/pairing-token

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  # Start the manager on a first install; on an upgrade, restart whatever was running.
  if systemctl is-enabled home-harness-manager.service >/dev/null 2>&1; then
    systemctl try-restart home-harness-manager.service home-harness-agent.service || true
  else
    systemctl enable --now home-harness-manager.service || true
  fi
fi
echo "Home Harness installed. Dashboard: http://127.0.0.1:7421 (sign-in link: journalctl -u home-harness-manager | grep '#login=')."
echo "To make this machine a worker too, fill in /etc/home-harness/agent.env and run: systemctl enable --now home-harness-agent"
