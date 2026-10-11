#!/usr/bin/env bash
# Lists Go files that are not gofmt-clean, ignoring line endings: the repo
# stores LF, but a Windows checkout with autocrlf has CRLF, which plain
# `gofmt -l` would report for every file. Exits 1 if any file needs
# formatting (run `gofmt -w` on it; gofmt keeps LF or CRLF as it finds it).
#
#   scripts/fmt-check.sh
set -euo pipefail
cd "$(dirname "$0")/.."
bad=0
while IFS= read -r f; do
  [[ -f "$f" ]] || continue # tracked but deleted in the working tree
  if [[ -n "$(tr -d '\r' <"$f" | gofmt -l)" ]]; then
    echo "$f"
    bad=1
  fi
done < <(git ls-files --cached --others --exclude-standard '*.go')
if ((bad)); then
  echo "the files above are not gofmt-formatted" >&2
  exit 1
fi
