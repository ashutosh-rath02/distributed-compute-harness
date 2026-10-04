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
#
# SIGNING: the Windows programs are Authenticode-signed when a certificate
# is configured (scripts/sign-windows.ps1 says how; without one they are
# unsigned, as before). Either way the bundle gets a signed release
# manifest — SHA256SUMS, SHA256SUMS.sig and release-key.pub — made with
# the release key ~/.home-harness/release-signing.key (created on first
# build; back it up; HARNESS_RELEASE_KEY overrides the path), which
# install.cmd checks before it touches the running manager. That protects
# the bundle in transit and at rest; only a code-signing certificate stops
# SmartScreen's warnings.
set -euo pipefail
cd "$(dirname "$0")/.."
out=dist/windows-manager
rm -rf "$out"
mkdir -p "$out/agents"

HARNESS_SIGN_NOTED=1 bash scripts/build-agents.sh >/dev/null
cp bin/agents/agent-* "$out/agents/"
# The release key's public half is built into manager.exe, so a manager
# from this bundle checks agent sets against it by itself.
release_pub="$(go run ./scripts/releasesign pubkey)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X main.releaseKey=$release_pub" -o "$out/manager.exe" ./cmd/manager
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o "$out/harnessctl.exe" ./cmd/harnessctl
cp scripts/windows-manager/install.ps1 scripts/windows-manager/install.cmd "$out/"
if [[ -f dist/home-harness-manager.apk ]]; then
  cp dist/home-harness-manager.apk "$out/home-harness.apk"
fi
bash scripts/sign-windows.sh "$out/manager.exe" "$out/harnessctl.exe"

# Last, once every file is final: the signed manifest of all of them.
go run ./scripts/releasesign sign -dir "$out" >/dev/null

# The manager refuses to start on an agent build it can't identify: vet the
# set with the same code before it ships — and the signed manifest, as the
# installer will.
checks=(); for a in "$out"/agents/agent-*; do checks+=(-agent-binary "$a"); done
if [[ -f "$out/home-harness.apk" ]]; then checks+=(-app-apk "$out/home-harness.apk"); fi
go run ./cmd/manager -check-agent-binaries -release-key "$out/release-key.pub" -release-manifest "$out/SHA256SUMS" "${checks[@]}" >/dev/null
echo "built $out (install: double-click install.cmd on the Windows PC)"
