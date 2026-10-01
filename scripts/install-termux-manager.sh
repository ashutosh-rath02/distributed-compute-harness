#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail

stage="${HOME_HARNESS_STAGE:-/sdcard/Download/HomeHarness}"
install_dir="$HOME/.home-harness"

if [[ ! -r "$stage/manager" || ! -r "$stage/agent.exe" ]]; then
  echo "Missing manager or agent.exe in $stage" >&2
  exit 1
fi

mkdir -p "$install_dir/bin" "$install_dir/state"
# Copy to new inodes first, then rename atomically. Android/Linux refuses
# to overwrite the inode of a binary that is still executing (ETXTBSY),
# while rename safely replaces the directory entry and lets the old
# process finish against its already-open inode.
cp "$stage/manager" "$install_dir/bin/manager.new"
chmod 700 "$install_dir/bin/manager.new"
mv -f "$install_dir/bin/manager.new" "$install_dir/bin/manager"
cp "$stage/agent.exe" "$install_dir/bin/agent.exe.new"
mv -f "$install_dir/bin/agent.exe.new" "$install_dir/bin/agent.exe"

if [[ ! -s "$install_dir/state/pairing-token" ]]; then
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$install_dir/state/pairing-token"
  chmod 600 "$install_dir/state/pairing-token"
fi

if [[ -s "$install_dir/state/manager.pid" ]]; then
  old_pid="$(cat "$install_dir/state/manager.pid")"
  if kill -0 "$old_pid" 2>/dev/null; then
    kill "$old_pid"
    for _ in 1 2 3 4 5; do
      kill -0 "$old_pid" 2>/dev/null || break
      sleep 1
    done
  fi
fi

pairing_token="$(cat "$install_dir/state/pairing-token")"
launcher="$install_dir/run-manager.sh"
cat > "$launcher" <<'LAUNCHER'
#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail
install_dir="$HOME/.home-harness"
pairing_token="$(cat "$install_dir/state/pairing-token")"
echo $$ > "$install_dir/state/manager.pid"
exec "$install_dir/bin/manager" \
  -addr :7420 \
  -api-addr 127.0.0.1:7421 \
  -pairing-token "$pairing_token" \
  -agent-binary "$install_dir/bin/agent.exe" \
  -db "$install_dir/state/manager.db" \
  -tls-dir "$install_dir/state/tls"
LAUNCHER
chmod 700 "$launcher"

mkdir -p "$HOME/.termux/boot"
cat > "$HOME/.termux/boot/start-home-harness-manager.sh" <<'BOOT'
#!/data/data/com.termux/files/usr/bin/bash
termux-wake-lock 2>/dev/null || true
install_dir="$HOME/.home-harness"
exec "$install_dir/run-manager.sh" > "$install_dir/state/manager.log" 2>&1
BOOT
chmod 700 "$HOME/.termux/boot/start-home-harness-manager.sh"

nohup "$launcher" > "$install_dir/state/manager.log" 2>&1 &
manager_pid=$!

sleep 2
if ! kill -0 "$manager_pid" 2>/dev/null; then
  echo "Manager failed to start:" >&2
  tail -n 30 "$install_dir/state/manager.log" >&2
  exit 1
fi

echo "Home Harness manager started (PID $manager_pid)."
echo "Open http://127.0.0.1:7421 in the phone browser."
echo "Log: $install_dir/state/manager.log"
