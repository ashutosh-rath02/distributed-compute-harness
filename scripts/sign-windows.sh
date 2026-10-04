#!/usr/bin/env bash
# Authenticode-signs Windows binaries in place when a code-signing
# certificate is configured (HARNESS_SIGN_THUMBPRINT, HARNESS_SIGN_PFX or
# HARNESS_SIGN_SIGNTOOL_ARGS — see scripts/sign-windows.ps1, which does the
# work); with none, the files stay unsigned, exactly as before, and it
# says so once per build (HARNESS_SIGN_NOTED=1 silences it, for a script
# that will say it itself).
#
#   scripts/sign-windows.sh FILE.exe...
#
# The build scripts call it right after building, before anything hashes
# the files: the agent catalog, the APK's bundled agents and the release
# manifest must all see the signed bytes.
set -euo pipefail

if [[ -z "${HARNESS_SIGN_THUMBPRINT:-}${HARNESS_SIGN_PFX:-}${HARNESS_SIGN_SIGNTOOL_ARGS:-}" ]]; then
  [[ -n "${HARNESS_SIGN_NOTED:-}" ]] ||
    echo "== Windows binaries are NOT code-signed (no certificate configured; see scripts/sign-windows.ps1)" >&2
  exit 0
fi
(($#)) || exit 0

# Signing was asked for: never ship unsigned because it can't run here.
if ! command -v powershell.exe >/dev/null 2>&1; then
  echo "Authenticode signing is configured (HARNESS_SIGN_*) but needs Windows PowerShell: build on Windows, or unset it" >&2
  exit 1
fi
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi; }
# Paths handed over in the environment must be Windows paths for PowerShell.
for v in HARNESS_SIGN_PFX HARNESS_SIGN_PFX_PASS_FILE HARNESS_SIGNTOOL HARNESS_SIGN_CACHE; do
  if [[ -n "${!v:-}" ]]; then export "$v=$(winpath "${!v}")"; fi
done
files=()
for f in "$@"; do files+=("$(winpath "$f")"); done
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass \
  -File "$(winpath "$(dirname "$0")/sign-windows.ps1")" "${files[@]}"
