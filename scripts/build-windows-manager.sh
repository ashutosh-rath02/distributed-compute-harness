#!/usr/bin/env bash
# Builds the Windows manager bundle — copy the folder to any Windows PC and
# double-click install.cmd to install (or update) the manager there:
#
#   scripts/build-windows-manager.sh    -> dist/windows-manager/
#
# It holds manager.exe, harnessctl.exe, every platform's agent build (what
# the manager hands to devices that join), the Android app if
# dist/home-harness-manager.apk exists (build it first, from the same
# commit, so app workers and the catalog match), and the installer.
set -euo pipefail
cd "$(dirname "$0")/.."
out=dist/windows-manager
rm -rf "$out"
mkdir -p "$out/agents"

bash scripts/build-agents.sh >/dev/null
cp bin/agents/agent-* "$out/agents/"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o "$out/manager.exe" ./cmd/manager
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o "$out/harnessctl.exe" ./cmd/harnessctl
cp scripts/windows-manager/install.ps1 scripts/windows-manager/install.cmd "$out/"
if [[ -f dist/home-harness-manager.apk ]]; then
  cp dist/home-harness-manager.apk "$out/home-harness.apk"
fi

# The manager refuses to start on an agent build it can't identify: vet the
# set with the same code before it ships.
checks=(); for a in "$out"/agents/agent-*; do checks+=(-agent-binary "$a"); done
go run ./cmd/manager -check-agent-binaries "${checks[@]}" >/dev/null
echo "built $out (install: double-click install.cmd on the Windows PC)"
