#!/bin/bash
# Builds an installer package for macOS: dist/HomeHarness-<version>.pkg.
# RUNS ONLY ON A MAC (needs pkgbuild and productbuild from Xcode's command line
# tools). Not tested by the author: only syntax-checked (bash -n) on Windows.
#
#   VERSION=1.0.0 bash packaging/macos/build-macos-pkg.sh
#
# Optional signing (without it macOS Gatekeeper will warn on first open):
#   SIGN_ID="Developer ID Installer: Your Name (TEAMID)"   signs the package
#   see scripts/sign-macos.sh for signing the programs themselves first.
#
# It builds the programs for both Mac CPU types and installs, via the package:
#   /usr/local/libexec/home-harness/{manager,agent,run-*.sh}   the programs (the plists run these)
#   /usr/local/bin/harnessctl                                  the command-line tool
#   /usr/local/share/home-harness/agents/                      other platforms' agents, handed to new devices
#   /Library/LaunchAgents/com.homeharness.{manager,agent}.plist
# The package's version of each program is for the Mac's own CPU: it picks the
# right one with a small launcher so one .pkg runs on Intel and Apple silicon.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
version="${VERSION:?set VERSION, e.g. VERSION=1.0.0}"
out="$repo/dist"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

command -v pkgbuild >/dev/null || { echo "build-macos-pkg.sh: pkgbuild not found: run this on a Mac with Xcode command line tools (xcode-select --install)" >&2; exit 1; }
command -v productbuild >/dev/null || { echo "build-macos-pkg.sh: productbuild not found" >&2; exit 1; }

root="$stage/root"
mkdir -p "$root/usr/local/libexec/home-harness" "$root/usr/local/bin" "$root/usr/local/share/home-harness/agents" "$root/Library/LaunchAgents"

build() { # program arch outfile
  (cd "$repo" && CGO_ENABLED=0 GOOS=darwin GOARCH="$2" go build -trimpath -ldflags "-s -w" -o "$3" "./cmd/$1")
}
for prog in manager agent harnessctl; do
  build "$prog" arm64 "$stage/$prog-arm64"
  build "$prog" amd64 "$stage/$prog-amd64"
  # A tiny launcher chooses the build for this Mac's CPU, so there is one
  # package for both (lipo would also work for a single fat binary).
  dest="$root/usr/local/libexec/home-harness/$prog"
  cp "$stage/$prog-arm64" "$dest-arm64"
  cp "$stage/$prog-amd64" "$dest-amd64"
  printf '#!/bin/bash\nd="$(cd "$(dirname "$0")" && pwd)"\nif [ "$(uname -m)" = arm64 ]; then exec "$d/%s-arm64" "$@"; else exec "$d/%s-amd64" "$@"; fi\n' "$prog" "$prog" > "$dest"
  chmod 755 "$dest" "$dest-arm64" "$dest-amd64"
done
ln -s ../libexec/home-harness/harnessctl "$root/usr/local/bin/harnessctl"

install -m 755 "$here/run-manager.sh" "$here/run-agent.sh" "$root/usr/local/libexec/home-harness/"
install -m 644 "$here/com.homeharness.manager.plist" "$here/com.homeharness.agent.plist" "$root/Library/LaunchAgents/"
if [ -d "$repo/bin/agents" ]; then cp "$repo"/bin/agents/* "$root/usr/local/share/home-harness/agents/" 2>/dev/null || true; fi

scripts="$stage/scripts"
mkdir -p "$scripts"
install -m 755 "$here/postinstall" "$scripts/postinstall"

mkdir -p "$out"
pkgbuild --root "$root" --scripts "$scripts" --identifier com.homeharness.pkg --version "$version" \
  --install-location / "$stage/HomeHarness-component.pkg"

sign=()
[ -n "${SIGN_ID:-}" ] && sign=(--sign "$SIGN_ID")
productbuild "${sign[@]+"${sign[@]}"}" --package "$stage/HomeHarness-component.pkg" "$out/HomeHarness-$version.pkg"
echo "Built $out/HomeHarness-$version.pkg"
