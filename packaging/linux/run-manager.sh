#!/bin/sh
# Starts the Home Harness manager under systemd (home-harness-manager.service).
# Installed as /usr/libexec/home-harness/run-manager.
#
# Why a wrapper: the manager takes its pairing token as a command-line flag,
# and a flag in a unit file is readable by every user via `systemctl show`.
# Here the token comes from a file only root and the service can read.
set -eu

state="${STATE_DIRECTORY:-/var/lib/home-harness/manager}"
conf="${HH_CONFIG_DIR:-/etc/home-harness}"
share="${HH_SHARE_DIR:-/usr/share/home-harness}"

token_file="$conf/pairing-token"
if [ ! -r "$token_file" ]; then
  echo "home-harness: cannot read $token_file (re-run the installer, or create it with a long random secret)" >&2
  exit 1
fi
token="$(tr -d ' \t\r\n' < "$token_file")"

agents=""
if [ -d "$share/agents" ]; then
  for f in "$share"/agents/*; do
    [ -f "$f" ] && agents="$agents -agent-binary $f"
  done
fi

# MANAGER_ARGS (manager.env) is split on spaces on purpose: it holds flags, not paths with spaces.
# shellcheck disable=SC2086
exec /usr/bin/home-harness-manager \
  -addr :7420 -api-addr 127.0.0.1:7421 -join-addr :7419 \
  -pairing-token "$token" \
  -db "$state/manager.db" -tls-dir "$state/tls" \
  -operator-token-file "$state/operator-token" -ai-key-file "$state/ai-key" \
  -remote-access-file "$state/remote-access" -artifact-dir "$state/artifacts" \
  $agents ${MANAGER_ARGS:-}
