#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail

# Stage the manager plus one agent build per platform you want this phone
# to onboard (from scripts/build-agents.sh, e.g. agent-windows-amd64.exe,
# agent-linux-arm64); a lone agent.exe from older instructions works too.
stage="${HOME_HARNESS_STAGE:-/sdcard/Download/HomeHarness}"
install_dir="$HOME/.home-harness"

shopt -s nullglob
staged_agents=("$stage"/agent*)
if [[ ! -r "$stage/manager" || ${#staged_agents[@]} -eq 0 ]]; then
  echo "Missing manager or any agent build (agent*) in $stage" >&2
  exit 1
fi

mkdir -p "$install_dir/bin/agents" "$install_dir/state"
# Copy to new inodes first, then rename atomically. Android/Linux refuses
# to overwrite the inode of a binary that is still executing (ETXTBSY),
# while rename safely replaces the directory entry and lets the old
# process finish against its already-open inode.
cp "$stage/manager" "$install_dir/bin/manager.new"
chmod 700 "$install_dir/bin/manager.new"

# Vet the staged builds with the new manager before touching the running
# one: a set it would refuse at startup (say, an old agent.exe left next
# to agent-windows-amd64.exe, two windows/amd64 builds) must not stop a
# working manager and leave Termux:Boot relaunching one that can't start.
# Run from $HOME, since /sdcard may be mounted noexec.
check_args=()
for agent in "$stage"/agent*.exe; do check_args+=(-agent-binary "$agent"); done
for agent in "${staged_agents[@]}"; do
  [[ "$agent" == *.exe ]] || check_args+=(-agent-binary "$agent")
done
if ! "$install_dir/bin/manager.new" -check-agent-binaries "${check_args[@]}"; then
  rm -f "$install_dir/bin/manager.new"
  echo "The agent builds staged in $stage can't be served together (see above)." >&2
  echo "Fix that folder (e.g. remove an old agent.exe) and run this again; the running manager was left untouched." >&2
  exit 1
fi
mv -f "$install_dir/bin/manager.new" "$install_dir/bin/manager"

# Mirror the staged agent builds exactly, so a platform dropped from the
# stage stops being served instead of lingering as a stale build.
for installed in "$install_dir"/bin/agents/agent*; do
  [[ -e "$stage/$(basename "$installed")" ]] || rm -f "$installed"
done
for agent in "${staged_agents[@]}"; do
  name="$(basename "$agent")"
  cp "$agent" "$install_dir/bin/agents/$name.new"
  mv -f "$install_dir/bin/agents/$name.new" "$install_dir/bin/agents/$name"
done

for secret in pairing-token operator-token; do
  if [[ ! -s "$install_dir/state/$secret" ]]; then
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$install_dir/state/$secret"
    chmod 600 "$install_dir/state/$secret"
  fi
done

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

launcher="$install_dir/run-manager.sh"
cat > "$launcher" <<'LAUNCHER'
#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail
shopt -s nullglob
install_dir="$HOME/.home-harness"
pairing_token="$(cat "$install_dir/state/pairing-token")"
# One -agent-binary per installed build, Windows builds first. (The
# manager picks its legacy-route build by platform, not order; listing
# Windows first just keeps the startup log readable.)
agent_args=()
for agent in "$install_dir"/bin/agents/*.exe; do agent_args+=(-agent-binary "$agent"); done
for agent in "$install_dir"/bin/agents/agent*; do
  [[ "$agent" == *.exe ]] || agent_args+=(-agent-binary "$agent")
done
echo $$ > "$install_dir/state/manager.pid"
exec "$install_dir/bin/manager" \
  -addr :7420 \
  -api-addr 127.0.0.1:7421 \
  -pairing-token "$pairing_token" \
  -operator-token-file "$install_dir/state/operator-token" \
  "${agent_args[@]}" \
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

# Sign the phone's browser in directly. The operator token rides in the
# link's fragment (never sent to a server) and the dashboard scrubs it from
# the address bar; opening it here avoids a copy-paste through the
# clipboard, which keyboards and foreground apps can read.
login_url="http://127.0.0.1:7421/#login=$(cat "$install_dir/state/operator-token")"
echo "Home Harness manager started (PID $manager_pid) serving ${#staged_agents[@]} agent build(s)."
if termux-open-url "$login_url" 2>/dev/null; then
  echo "Opened the dashboard sign-in link in the phone browser."
else
  echo "Open this private sign-in link in the phone browser: $login_url"
fi
echo "harnessctl on this phone: export HARNESS_OPERATOR_TOKEN=\$(cat $install_dir/state/operator-token)"
echo "Log: $install_dir/state/manager.log"
