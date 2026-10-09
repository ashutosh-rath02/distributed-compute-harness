#!/bin/bash
# Starts the Home Harness agent for the logged-in user (com.homeharness.agent).
# Installed as /usr/local/libexec/home-harness/run-agent.sh.
#
# It waits (quietly, checking every minute) until
#   ~/Library/Application Support/HomeHarness/agent.args
# exists. Put the flags that join a manager in it, for example:
#   -manager-addr <host>:7420 -manager-fingerprint <fingerprint> -pairing-token <token>
# or, to be approved on the manager's dashboard instead of using a token:
#   -manager-addr <host>:7420 -manager-fingerprint <fingerprint> -pair
# (The dashboard's "Add a device" page shows the values; never share a real token.)
set -eu

base="${HH_SUPPORT_DIR:-$HOME/Library/Application Support/HomeHarness}"
prefix="${HH_PREFIX:-/usr/local}"
libexec="${HH_LIBEXEC:-$prefix/libexec/home-harness}"
mkdir -p "$base"
args="$base/agent.args"

while [ ! -s "$args" ]; do sleep 60; done
chmod 600 "$args"
# shellcheck disable=SC2207
flags=($(tr '\n' ' ' < "$args"))

exec "$libexec/agent" -no-self-update \
  -identity-dir "$base/agent-identity" -work-dir "$base/work" \
  ${flags[@]+"${flags[@]}"} >> "$base/agent.log" 2>&1
