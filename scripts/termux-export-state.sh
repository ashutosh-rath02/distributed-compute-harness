#!/data/data/com.termux/files/usr/bin/bash
# Moves the Termux-hosted manager to the Home Harness Android app (roadmap
# item 9) without re-onboarding anything: exports its state — tokens,
# database, and the TLS certificate every agent has pinned — for the app
# to import, and retires the Termux manager so the two never run at once.
#
#   bash termux-export-state.sh [/sdcard/Download/home-harness-state.zip]
#
# Needs storage access once: termux-setup-storage.
set -euo pipefail
install_dir="$HOME/.home-harness"
state="$install_dir/state"
out="${1:-/sdcard/Download/home-harness-state.zip}"
boot="$HOME/.termux/boot/start-home-harness-manager.sh"

stage="${HOME_HARNESS_STAGE:-/sdcard/Download/HomeHarness}"

[[ -d "$state" ]] || { echo "No Termux manager state at $state." >&2; exit 1; }
[[ -d "$(dirname "$out")" ]] || { echo "Can't write to $(dirname "$out"): run termux-setup-storage and allow storage access first." >&2; exit 1; }

# A manager installed before the app existed can't export. Then use a
# current linux/arm64 manager build from the stage folder (copied into
# $HOME first: /sdcard may be mounted noexec). Checked before anything is
# stopped, so a failure here leaves the running manager alone.
can_export() { local help; help="$("$1" -help 2>&1 || true)"; [[ "$help" == *-export-state* ]]; }
mgr="$install_dir/bin/manager"
if ! can_export "$mgr"; then
  if [[ -r "$stage/manager" ]]; then
    cp "$stage/manager" "$install_dir/bin/manager-export"
    chmod 700 "$install_dir/bin/manager-export"
    mgr="$install_dir/bin/manager-export"
  fi
  if ! can_export "$mgr"; then
    rm -f "$install_dir/bin/manager-export"
    echo "This Termux manager is too old to export its state. Copy a current" >&2
    echo "linux/arm64 manager build to $stage/manager and run this again." >&2
    echo "(Nothing was changed; the Termux manager is still running.)" >&2
    exit 1
  fi
fi

# Stop the manager, and keep the boot script from starting it again: two
# managers on one phone fight over port 7420.
if [[ -s "$state/manager.pid" ]]; then
  pid="$(cat "$state/manager.pid")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  fi
fi
if [[ -f "$boot" ]]; then
  mv "$boot" "$boot.moved-to-app"
  echo "Disabled the Termux boot start ($boot -> $boot.moved-to-app)."
fi

"$mgr" -export-state "$out" -state-dir "$state"
rm -f "$install_dir/bin/manager-export"
chmod 600 "$out" 2>/dev/null || true

cat <<EOF

Next, on this phone:
  1. Install the Home Harness app (home-harness-manager.apk) and open it.
  2. Menu > "Import state (from Termux)…" and pick $out.
  3. The app offers to delete the file afterwards: say yes. It holds the
     manager's TLS key and tokens.
Your devices reconnect to the app on their own (same address, same
certificate, same tokens).

To go back to the Termux manager instead (never both at once): rename
$boot.moved-to-app back to $boot, then reboot or run
  nohup $install_dir/run-manager.sh > $state/manager.log 2>&1 &
EOF
