#!/usr/bin/env bash
# Signs (Developer ID, hardened runtime) and notarizes macOS builds in
# place, on a Mac with Xcode's command line tools, when configured;
# unconfigured, or anywhere but a Mac, it changes nothing.
# scripts/build-agents.sh runs it right after building, before anything
# hashes the files.
#
#   scripts/sign-macos.sh FILE...
#
#   HARNESS_MACOS_SIGN_IDENTITY  the signing identity in your keychain, e.g.
#                                "Developer ID Application: Your Name (TEAMID)"
# and, to notarize (Gatekeeper refuses un-notarized programs a browser
# downloaded), one of:
#   HARNESS_NOTARY_PROFILE       a keychain profile saved once with
#                                xcrun notarytool store-credentials NAME --key KEY.p8 --key-id ID --issuer ISSUER
#   HARNESS_NOTARY_KEY           an App Store Connect API key (.p8), with
#   + HARNESS_NOTARY_KEY_ID      its key ID and
#   + HARNESS_NOTARY_ISSUER      its issuer ID
#
# Each file is signed with a secure timestamp and checked (codesign
# --verify --strict); then all of them go to Apple's notary service in one
# zip, and the build waits for the verdict and fails unless it is
# Accepted, printing Apple's log.
#
# No stapling: a ticket can be stapled only to an .app, .pkg or .dmg, not
# to a bare executable like these. Gatekeeper instead looks the ticket up
# online the first time a quarantined copy runs (one a browser
# downloaded). Copies fetched by curl — the join scripts — or by the
# agent's own self-update carry no quarantine flag, so Gatekeeper never
# checks them; Apple Silicon only needs them signed, which the Go linker
# already does (ad hoc).
set -euo pipefail

identity="${HARNESS_MACOS_SIGN_IDENTITY:-}"
[[ -n "$identity" ]] || exit 0
if [[ "$(uname -s)" != Darwin ]]; then
  echo "== HARNESS_MACOS_SIGN_IDENTITY is set, but macOS signing needs a Mac: the darwin builds stay unsigned" >&2
  exit 0
fi
(($#)) || exit 0

for f in "$@"; do
  codesign --force --options runtime --timestamp --sign "$identity" "$f"
  codesign --verify --strict --verbose=2 "$f"
done

if [[ -n "${HARNESS_NOTARY_PROFILE:-}" ]]; then
  notary=(--keychain-profile "$HARNESS_NOTARY_PROFILE")
elif [[ -n "${HARNESS_NOTARY_KEY:-}" ]]; then
  notary=(--key "$HARNESS_NOTARY_KEY" --key-id "${HARNESS_NOTARY_KEY_ID:?set HARNESS_NOTARY_KEY_ID}"
    --issuer "${HARNESS_NOTARY_ISSUER:?set HARNESS_NOTARY_ISSUER}")
else
  echo "== signed but NOT notarized (set HARNESS_NOTARY_PROFILE, or HARNESS_NOTARY_KEY + _KEY_ID + _ISSUER): Gatekeeper will refuse copies a browser downloads" >&2
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/builds"
cp "$@" "$tmp/builds/"
ditto -c -k --keepParent "$tmp/builds" "$tmp/builds.zip"
echo "== notarizing $* (this waits for Apple, usually a few minutes)"
out="$(xcrun notarytool submit "$tmp/builds.zip" "${notary[@]}" --wait --output-format json)" || true
if ! grep -q '"status" *: *"Accepted"' <<<"$out"; then
  echo "notarization failed: $out" >&2
  id="$(sed -n 's/.*"id" *: *"\([^"]*\)".*/\1/p' <<<"$out")"
  [[ -z "$id" ]] || xcrun notarytool log "$id" "${notary[@]}" >&2 || true
  exit 1
fi
echo "notarized $*"
