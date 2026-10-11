#!/bin/sh
# Installs Home Harness as system services on Linux (systemd): the manager,
# the agent, or both. For a package instead (.deb / .rpm) see nfpm.yaml.
#
#   sudo sh install.sh --bin-dir ./bin [--manager] [--agent] [--agents-dir ./bin/agents]
#
# --bin-dir     folder holding the built programs: manager, agent, harnessctl
#               (go build -o bin/manager ./cmd/manager, and so on)
# --agents-dir  agent builds for OTHER platforms the manager should hand out to
#               new devices (scripts/build-agents.sh makes them); optional
# --manager / --agent   what to install (default: both)
# --no-start    install but do not enable or start anything
#
# It creates the users homeharness (manager) and homeharness-agent, installs the
# programs under /usr/bin, and puts configuration in /etc/home-harness and
# data in /var/lib/home-harness. It never overwrites your configuration.
# Remove it again with uninstall.sh.
#
# For testing, HH_ROOT=/some/dir installs into that folder instead of / and
# touches no users or services.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
root="${HH_ROOT:-}"
bin_dir=""
agents_dir=""
want_manager=0
want_agent=0
start=1

while [ $# -gt 0 ]; do
  case "$1" in
    --bin-dir) bin_dir="$2"; shift 2 ;;
    --agents-dir) agents_dir="$2"; shift 2 ;;
    --manager) want_manager=1; shift ;;
    --agent) want_agent=1; shift ;;
    --no-start) start=0; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "install.sh: unknown option $1 (try --help)" >&2; exit 2 ;;
  esac
done
[ "$want_manager$want_agent" = "00" ] && { want_manager=1; want_agent=1; }
[ -n "$bin_dir" ] || { echo "install.sh: --bin-dir is required: the folder with the built manager, agent and harnessctl" >&2; exit 2; }

if [ -z "$root" ] && [ "$(id -u)" -ne 0 ]; then
  echo "install.sh: run it as root (sudo sh install.sh ...): it installs system services." >&2
  exit 1
fi
if [ -z "$root" ] && ! command -v systemctl >/dev/null 2>&1; then
  echo "install.sh: this needs systemd. Without it, run the programs yourself or use the join script from the dashboard." >&2
  exit 1
fi

need() { [ -f "$bin_dir/$1" ] || { echo "install.sh: $bin_dir/$1 not found" >&2; exit 1; }; }
[ "$want_manager" = 1 ] && { need manager; need harnessctl; }
[ "$want_agent" = 1 ] && need agent

mkuser() { # name
  [ -n "$root" ] && return 0
  id "$1" >/dev/null 2>&1 && return 0
  nologin=/usr/sbin/nologin; [ -x "$nologin" ] || nologin=/sbin/nologin; [ -x "$nologin" ] || nologin=/bin/false
  useradd --system --no-create-home --home-dir /nonexistent --shell "$nologin" "$1"
}

mkdirm() { # mode dir
  mkdir -p "$2"
  chmod "$1" "$2" 2>/dev/null || [ -n "$root" ] || { echo "install.sh: cannot set permissions on $2" >&2; exit 1; }
}

for d in etc/home-harness usr/bin usr/libexec/home-harness usr/lib/systemd/system; do mkdirm 0755 "$root/$d"; done

if [ "$want_manager" = 1 ]; then
  mkuser homeharness
  install -m 0755 "$bin_dir/manager" "$root/usr/bin/home-harness-manager"
  install -m 0755 "$bin_dir/harnessctl" "$root/usr/bin/harnessctl"
  install -m 0755 "$here/run-manager.sh" "$root/usr/libexec/home-harness/run-manager"
  install -m 0644 "$here/home-harness-manager.service" "$root/usr/lib/systemd/system/home-harness-manager.service"
  [ -f "$root/etc/home-harness/manager.env" ] || install -m 0644 "$here/manager.env" "$root/etc/home-harness/manager.env"
  mkdirm 0755 "$root/usr/share/home-harness/agents"
  if [ -n "$agents_dir" ]; then
    for f in "$agents_dir"/*; do [ -f "$f" ] && install -m 0644 "$f" "$root/usr/share/home-harness/agents/$(basename "$f")"; done
  fi
  if [ ! -f "$root/etc/home-harness/pairing-token" ]; then
    umask 077
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$root/etc/home-harness/pairing-token"
    umask 022
  fi
  chmod 0640 "$root/etc/home-harness/pairing-token"
  [ -n "$root" ] || chgrp homeharness "$root/etc/home-harness/pairing-token"
  mkdirm 0700 "$root/var/lib/home-harness/manager"
  [ -n "$root" ] || chown homeharness:homeharness "$root/var/lib/home-harness/manager"
fi

if [ "$want_agent" = 1 ]; then
  mkuser homeharness-agent
  install -m 0755 "$bin_dir/agent" "$root/usr/bin/home-harness-agent"
  install -m 0644 "$here/home-harness-agent.service" "$root/usr/lib/systemd/system/home-harness-agent.service"
  if [ ! -f "$root/etc/home-harness/agent.env" ]; then
    install -m 0600 "$here/agent.env" "$root/etc/home-harness/agent.env"
  fi
  mkdirm 0700 "$root/var/lib/home-harness/agent"
  [ -n "$root" ] || chown homeharness-agent:homeharness-agent "$root/var/lib/home-harness/agent"
fi

if [ -n "$root" ]; then
  echo "Installed into $root (staging only: no users created, nothing started)."
  exit 0
fi

systemctl daemon-reload
if [ "$want_manager" = 1 ] && [ "$start" = 1 ]; then
  systemctl enable --now home-harness-manager.service
  echo
  echo "Manager started. Its dashboard is at http://127.0.0.1:7421 on this machine."
  echo "Sign-in link (keep it private): journalctl -u home-harness-manager | grep '#login='"
  echo "Operator token for harnessctl:  sudo cat /var/lib/home-harness/manager/operator-token"
  echo "Pairing token for new devices:  sudo cat /etc/home-harness/pairing-token"
fi
if [ "$want_agent" = 1 ]; then
  echo
  echo "Agent installed but not started: edit /etc/home-harness/agent.env to say which manager to join,"
  echo "then: sudo systemctl enable --now home-harness-agent"
fi
