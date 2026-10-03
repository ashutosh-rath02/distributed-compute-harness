#!/usr/bin/env bash
# Cross-compiles the agent for every platform the manager's catalog can
# serve, into bin/agents/. Pass each file to the manager with its own
# -agent-binary flag; the platform is read from the file itself.
#
#   scripts/build-agents.sh
#   manager -pairing-token ... -agent-binary bin/agents/agent-windows-amd64.exe \
#           -agent-binary bin/agents/agent-linux-arm64 ...
#
# CGO_ENABLED=0 keeps every build static and buildable from any host
# (including this project's Windows dev machine, which has no C toolchain).
#
# Android/Termux agents are deliberately GOOS=linux builds: a GOOS=android
# build would report itself as android/arm64, match no catalog entry, and
# silently never be offered self-updates.
set -euo pipefail

cd "$(dirname "$0")/.."
out="bin/agents"
mkdir -p "$out"

targets=(
  "windows amd64 agent-windows-amd64.exe"
  "linux arm64 agent-linux-arm64"   # Android (Termux) and ARM Linux
  "linux arm agent-linux-arm"       # 32-bit Android devices (older tablets)
  "linux amd64 agent-linux-amd64"
  "darwin arm64 agent-darwin-arm64"
  "darwin amd64 agent-darwin-amd64"
)

failed=()
for target in "${targets[@]}"; do
  read -r goos goarch name <<<"$target"
  if CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM=7 go build -trimpath -o "$out/$name" ./cmd/agent; then
    echo "built $out/$name ($goos/$goarch)"
  else
    failed+=("$goos/$goarch")
  fi
done

if ((${#failed[@]})); then
  echo "FAILED to build: ${failed[*]}" >&2
  exit 1
fi
