#!/usr/bin/env bash
# Source this from a build script to get the build's identity:
#
#   source "$(dirname "$0")/version.sh"
#   go build -ldflags "$VERSION_LDFLAGS" ./cmd/agent
#
# It sets:
#   HARNESS_VERSION  $HARNESS_VERSION if you set it (a release passes the tag
#                    without its "v"), else the tag when HEAD is exactly on a
#                    vX.Y.Z tag, else 0.0.0-dev+<short commit>
#   HARNESS_COMMIT   the full commit hash ("unknown" outside a git checkout)
#   HARNESS_DATE     the commit's time, UTC (not the wall clock, so rebuilding
#                    one commit gives the same bytes; $HARNESS_DATE overrides)
#   VERSION_LDFLAGS  the -X flags for internal/version
#
# Updates never compare version strings (they compare binary hashes), so a
# dev build changing its version with every commit costs nothing.

HARNESS_COMMIT="${HARNESS_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
if [[ -z "${HARNESS_VERSION:-}" ]]; then
  tag="$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null || true)"
  if [[ -n "$tag" ]]; then
    HARNESS_VERSION="${tag#v}"
  else
    HARNESS_VERSION="0.0.0-dev+${HARNESS_COMMIT:0:7}"
  fi
fi
HARNESS_DATE="${HARNESS_DATE:-$(git show -s --format=%cI HEAD 2>/dev/null | head -n1 || true)}"
if [[ -n "$HARNESS_DATE" ]]; then
  # Normalise to UTC when GNU/BSD date can; otherwise keep git's own form.
  utc="$(date -u -d "$HARNESS_DATE" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"
  HARNESS_DATE="${utc:-$HARNESS_DATE}"
fi
export HARNESS_VERSION HARNESS_COMMIT HARNESS_DATE
VERSION_LDFLAGS="-X home-harness/internal/version.Version=$HARNESS_VERSION -X home-harness/internal/version.Commit=$HARNESS_COMMIT -X home-harness/internal/version.Date=$HARNESS_DATE"
