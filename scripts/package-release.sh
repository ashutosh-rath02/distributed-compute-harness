#!/usr/bin/env bash
# Builds everything a release ships into dist/release/ (release.yml and
# `make release-local` both run this):
#
#   home-harness_<version>_<os>_<arch>.tar.gz|zip  manager, agent, relay, harnessctl
#   home-harness-agents_<version>.tar.gz           every agent build the manager can serve
#   home-harness-windows-manager_<version>.zip     the double-click Windows manager bundle
#   SHA256SUMS, SHA256SUMS.sig, release-key.pub    the signed manifest of all of the above
#
# Version: HARNESS_VERSION (the tag without its "v"), else scripts/version.sh's
# default. SIGNING: the manifest is signed with the key at HARNESS_RELEASE_KEY
# (default ~/.home-harness/release-signing.key). Set HARNESS_REQUIRE_KEY=1 (CI
# does) to fail when that key file is missing instead of silently creating a
# new one, which would produce a release no installed PC trusts.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/version.sh

if [[ "${HARNESS_REQUIRE_KEY:-}" == 1 ]]; then
  key="${HARNESS_RELEASE_KEY:-$HOME/.home-harness/release-signing.key}"
  [[ -s "$key" ]] || { echo "release key $key is missing; refusing to create a new one" >&2; exit 1; }
fi

ver="$HARNESS_VERSION"
out=dist/release
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
rm -rf "$out"
mkdir -p "$out"

mkzip() { # zip-file dir : the directory's contents, without a wrapper folder
  local zipfile="$PWD/$1"
  if command -v zip >/dev/null 2>&1; then
    (cd "$2" && zip -qr "$zipfile" .)
  else
    python3 -m zipfile -c "$zipfile" "$2"/* 2>/dev/null || python -m zipfile -c "$zipfile" "$2"/*
  fi
}

echo "== agents for every platform"
bash scripts/build-agents.sh >/dev/null
bash scripts/check-agents.sh >/dev/null
tar -czf "$out/home-harness-agents_${ver}.tar.gz" -C bin/agents .

echo "== per-platform binaries"
for target in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64" "windows amd64"; do
  read -r goos goarch <<<"$target"
  ext=""; [[ $goos == windows ]] && ext=".exe"
  dir="$stage/${goos}_${goarch}"
  mkdir -p "$dir"
  for cmd in manager agent relay harnessctl; do
    flags="$VERSION_LDFLAGS"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags "$flags" -o "$dir/$cmd$ext" ./cmd/$cmd
  done
  name="home-harness_${ver}_${goos}_${goarch}"
  if [[ $goos == windows ]]; then mkzip "$out/$name.zip" "$dir"; else tar -czf "$out/$name.tar.gz" -C "$dir" .; fi
  echo "packaged $name"
done

echo "== Windows manager bundle"
HARNESS_VERSION="$ver" bash scripts/build-windows-manager.sh >/dev/null
mkzip "$out/home-harness-windows-manager_${ver}.zip" dist/windows-manager

echo "== signed manifest"
go run ./scripts/releasesign sign -dir "$out"
go run ./scripts/releasesign verify -dir "$out"
echo "release files in $out"
