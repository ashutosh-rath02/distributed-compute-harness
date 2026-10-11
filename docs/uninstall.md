# Uninstall

Every uninstaller **keeps your data** unless you ask for it to be deleted. The data you may care about:

- the **manager's state**: database, operator token, certificate and key, stored files. Deleting it is permanent: devices that trusted this manager will not trust a new one. To keep a copy first, stop the manager and run `manager -export-state state.zip -state-dir <its state folder>` (the file holds keys and tokens, so keep it private).
- the **agent's identity**: the device's name and keys with the manager. If you delete it, the device must be approved again.

## Windows

Run from a PowerShell window (no administrator needed):

```
powershell -ExecutionPolicy Bypass -File packaging\windows\uninstall.ps1
```

It asks before it does anything. It removes the scheduled tasks `HomeComputeHarnessAgent` and `HomeComputeHarnessManager` (and the older sign-in registry entry), stops their launcher loops and programs, and deletes the program folders (`%LOCALAPPDATA%\HomeHarness`, and the manager inside it). Options:

| Option | Meaning |
|---|---|
| `-Component agent` or `manager` | remove only one (default: both) |
| `-RemoveState` | also delete the manager's state and the agent identity (`%APPDATA%\home-harness\agent`) |
| `-WhatIf` | show what it would do, change nothing |
| `-Yes` | do not ask (for scripts) |
| `-AgentDir`, `-ManagerDir`, `-IdentityDir`, `-AgentTaskName`, `-ManagerTaskName` | for installs in non-standard places |

It refuses to delete a folder that is plainly not an install folder (your profile, AppData, the drive root). Windows Firewall rules for `agent.exe` and `manager.exe`, if you created them, stay: remove them in Windows Security, Firewall, Allowed apps. If you installed with winget, run `winget uninstall HomeHarness.HomeHarness` as well.

## Linux

**Package install (apt or rpm) or `packaging/linux/install.sh`:**

```
sudo sh packaging/linux/uninstall.sh            # removes services and programs, keeps /etc/home-harness and /var/lib/home-harness
sudo sh packaging/linux/uninstall.sh --purge    # also deletes configuration, data and the service users
```

With a package manager: `sudo apt remove home-harness` or `sudo dnf remove home-harness` (data stays; `apt purge` also removes the configuration files the package owns; delete `/var/lib/home-harness` yourself to remove the data).

**Agent installed by the dashboard's one-line script** (a per-user service, not the package):

```
systemctl --user disable --now home-harness-agent
rm -f ~/.config/systemd/user/home-harness-agent.service
rm -rf ~/.local/share/home-harness
crontab -l | grep -v 'home-harness/run-agent.sh' | crontab -     # only if it used the cron fallback
```

The agent identity is kept inside `~/.config/home-harness/agent` (delete it to start fresh).

## macOS

```
bash packaging/macos/uninstall.sh            # removes programs and launch agents, keeps your data
bash packaging/macos/uninstall.sh --purge    # also deletes ~/Library/Application Support/HomeHarness
```

Run it as yourself, not with sudo; it asks for your password only for the files in `/usr/local` and `/Library`. It also removes the per-user launch agent (`com.homeharness.agent`) the dashboard's join script installs. With Homebrew: `brew services stop home-harness && brew uninstall home-harness`; your data stays in `~/Library/Application Support/HomeHarness`.

## Android (Termux)

```
pkill -f home-harness
rm -rf ~/home-harness ~/.termux/boot/start-harness-agent.sh
```

If you installed the Android app, uninstall it like any app (Settings, Apps).

## Docker

```
docker compose -f packaging/docker/docker-compose.yml down        # stop and remove the containers, keep the data volume
docker compose -f packaging/docker/docker-compose.yml down -v     # also delete the data volume (the manager's state)
```

## After removing a device

The manager still lists a device that was uninstalled, as OFFLINE. Remove it from the fleet with `harnessctl revoke <id>` (it will be refused if it ever comes back), or leave it.
