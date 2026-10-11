#!/usr/bin/env bash
# Vets the agent builds in bin/agents/ (default) with the manager's own
# -check-agent-binaries: platform detection, no conflicts or duplicates.
# CI runs it after scripts/build-agents.sh; so can you.
#
#   scripts/check-agents.sh [dir]
set -euo pipefail
cd "$(dirname "$0")/.."
dir="${1:-bin/agents}"
checks=()
for a in "$dir"/agent-*; do
  [[ -e "$a" ]] || { echo "no agent builds in $dir (run scripts/build-agents.sh)" >&2; exit 1; }
  checks+=(-agent-binary "$a")
done
go run ./cmd/manager -check-agent-binaries "${checks[@]}"
