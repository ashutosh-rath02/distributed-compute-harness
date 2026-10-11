# Quickstart

About five minutes, two computers on the same Wi-Fi or network. You need one that stays on (it will be the **manager**) and one to add (the **worker**). They can be the same machine for a first look.

## 1. Install the manager

Pick the machine that will coordinate the others.

**Windows.** Copy the Windows bundle folder to the PC (or build it with `scripts/build-windows-manager.sh`) and double-click `install.cmd`. It needs no administrator rights. It installs into `%LOCALAPPDATA%\HomeHarness\manager`, starts the manager when you sign in, and opens the dashboard. If Windows asks whether `manager.exe` may accept network connections, allow it on private networks, so your other devices can reach it.

**Linux.** Build the programs, then run the installer as root. It creates a hardened systemd service:

```
go build -o bin/manager ./cmd/manager
go build -o bin/agent ./cmd/agent
go build -o bin/harnessctl ./cmd/harnessctl
sudo sh packaging/linux/install.sh --bin-dir ./bin --manager
```

**macOS.** Build and run it for your user (a launch agent starts it at login): see `packaging/macos/` for the package and launch agent files.

**Docker.** `docker compose -f packaging/docker/docker-compose.yml up -d --build` after filling in `.env` from `.env.example`. See [Uninstall](uninstall.md) for how to remove any of these again.

Open the dashboard at **http://127.0.0.1:7421** on that machine. The first time, use the sign-in link the manager prints when it starts (it ends in `#login=...` and carries a secret, so keep it to yourself). Windows: it opens by itself. Linux: `journalctl -u home-harness-manager | grep '#login='`.

## 2. Add a laptop

On the dashboard, click **Allow new devices for 15 minutes**. The dashboard shows an address like `http://192.168.1.20:7419`.

On the laptop, open that address in a browser. It shows one command for that laptop's operating system. Paste it into a terminal:

- Windows: a `powershell ... irm ... | iex` line.
- Mac or Linux: a `curl ... | bash` line.

The command downloads the agent, checks its checksum, starts it, and prints a **pairing code** (six digits). Back on the dashboard a card appears: "Waiting for approval", with the same six digits. If the digits match, click **Approve**. Within a couple of seconds the laptop shows up as **READY**.

Only add devices on a network you trust. The join page is plain HTTP so no device has to click through certificate warnings; the matching codes are what protect which manager the laptop trusts and which laptop gets in.

## 3. Run a task

Install nothing more: `harnessctl` came with the manager. From the manager's folder (where `harness-operator-token` lives; on Linux pass `-token-file`, see below):

```
harnessctl nodes
harnessctl tasks
harnessctl do cpu.burn seconds=10
```

`nodes` shows your laptop. `tasks` lists what the fleet can run. `do` runs one task on whichever device has room and prints the result. A file task looks like this:

```
harnessctl do -in photo.jpg image.resize width=800
```

If `harnessctl` says it cannot sign in, give it the token: `harnessctl -token-file /var/lib/home-harness/manager/operator-token nodes` (Linux package), or set `HARNESS_OPERATOR_TOKEN`.

## 4. Where next

- Many files at once: a **job** (`harnessctl map ...`), or a **workflow** of steps. See [Concepts](concepts.md).
- Use it from code: [Python and JavaScript clients](clients.md).
- Ask a local AI model: install [Ollama](https://ollama.com) on one device, then `harnessctl ask "What is the capital of France?"`.
- Something not working? [Troubleshooting](troubleshooting.md).
