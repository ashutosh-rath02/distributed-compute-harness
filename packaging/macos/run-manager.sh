#!/bin/bash
# Starts the Home Harness manager for the logged-in user (com.homeharness.manager).
# Installed as /usr/local/libexec/home-harness/run-manager.sh.
# Data lives in ~/Library/Application Support/HomeHarness/manager; the
# pairing token is created there on first start and never leaves your Mac.
set -eu

base="${HH_SUPPORT_DIR:-$HOME/Library/Application Support/HomeHarness}"
state="$base/manager"
prefix="${HH_PREFIX:-/usr/local}"
libexec="${HH_LIBEXEC:-$prefix/libexec/home-harness}"
mkdir -p "$state"
chmod 700 "$state"

if [ ! -s "$state/pairing-token" ]; then
  ( umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$state/pairing-token" )
fi
token="$(tr -d ' \t\r\n' < "$state/pairing-token")"

agents=()
if [ -d "$prefix/share/home-harness/agents" ]; then
  for f in "$prefix"/share/home-harness/agents/*; do
    [ -f "$f" ] && agents+=(-agent-binary "$f")
  done
fi

# Extra flags, one per line or space separated, in manager.args (optional).
extra=()
if [ -f "$base/manager.args" ]; then
  # shellcheck disable=SC2207
  extra=($(tr '\n' ' ' < "$base/manager.args"))
fi

exec "$libexec/manager" \
  -addr :7420 -api-addr 127.0.0.1:7421 -join-addr :7419 \
  -pairing-token "$token" \
  -db "$state/manager.db" -tls-dir "$state/tls" \
  -operator-token-file "$state/operator-token" -ai-key-file "$state/ai-key" \
  -remote-access-file "$state/remote-access" -artifact-dir "$state/artifacts" \
  ${agents[@]+"${agents[@]}"} ${extra[@]+"${extra[@]}"} \
  >> "$state/manager.log" 2>&1
