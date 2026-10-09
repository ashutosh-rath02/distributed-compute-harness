# Troubleshooting

Start with `harnessctl nodes` and the dashboard. Most problems show up there as a device that is OFFLINE, missing, or never READY.

## A new device never appears on the dashboard

1. **The join window is closed.** Adding devices works only while the window is open. Click **Allow new devices for 15 minutes** on the dashboard (or `harnessctl join-window open`). A new device that asks while it is closed is turned away quietly and asks again every 15 seconds, so opening the window is enough.
2. **The join page is not reachable.** Open the address shown on the dashboard from the new device's browser. If it does not load, the manager's firewall is blocking port **7419** (join page) or **7420** (devices). On Windows allow `manager.exe` on **private** networks when the firewall asks; if you dismissed that prompt, add the rule in Windows Security, Firewall, Allowed apps. On Linux open the ports (`ufw allow 7419,7420/tcp`). Ports 7419 and 7420 must be reachable from the device; 7421 (the API) stays on the manager's own machine.
3. **Different networks.** The device and manager must be on the same network (the same Wi-Fi, not a guest network that isolates clients). Otherwise use the relay.
4. **The code does not match.** If the six digits on the device and on the dashboard differ, do not approve. Something is between them, or the device is talking to a different manager. Stop and check the address.

## "Waiting for approval" but no card on the dashboard

The card is held in memory and dropped if the device stops asking for 30 seconds, or after 15 minutes, or when the manager restarts. Run the join command on the device again.

## The join command fails on Windows

- **Paste it into PowerShell, not an app that edits quotes.** If it is split across several lines when pasted, the installer still copes, but a mangled quote will not: copy it again from the join page.
- **Execution policy.** The command already passes `-ExecutionPolicy Bypass`; a managed PC that forbids that cannot run it. Download the agent from the join page by hand and run it with the flags `harnessctl join` prints.
- **SmartScreen or antivirus quarantines `agent.exe`.** The programs are not code-signed unless you signed them (see `scripts/sign-windows.ps1`). Allow it, or build and sign your own.
- **Re-running it is safe.** It stops the earlier agent first and replaces it.

## A device went offline

- **Wi-Fi changed or the router handed out a new address.** Agents installed from the dashboard find the manager again by LAN discovery, so it usually returns on its own within a minute. If the **manager's** address changed, a device with a fixed address in its settings keeps trying the old one: re-run the join command, or give the manager a fixed address (a DHCP reservation in your router is the simplest fix).
- **The laptop is asleep.** A sleeping computer is an offline device; it reconnects when it wakes. The manager asks the system not to sleep while it has work in flight, but it cannot keep an idle PC awake, and **a sleeping manager machine means the whole fleet is offline**. Put the manager on a computer that stays on, or change its power settings.
- **The agent stopped.** On Windows it is a per-user scheduled task named `HomeComputeHarnessAgent` that starts at sign-in and every two minutes as a watchdog. Check it in Task Scheduler. On Linux: `systemctl --user status home-harness-agent` (join script) or `systemctl status home-harness-agent` (package).
- **Heartbeats stopped.** The manager marks a device OFFLINE after 15 seconds without one and closes its connection.

## A task is stuck QUEUED

The dashboard and `harnessctl workloads` show why in a "waiting" note: every eligible device is busy, on battery, paused or outside its allowed hours, or no ready device offers that task type or has the model. Adjust with `harnessctl avail <id> always`, or add a device.

## A task is refused: "not allowed"

The policy has that type off, or raw commands are disabled (the default). Look with `harnessctl policy`; allow a type with `harnessctl policy type <name> on`. For raw commands, restrict them to a labelled device.

## The dashboard asks to sign in or shows "authentication required"

You need the manager's sign-in link (it ends in `#login=...`) or, for the CLI and API, the operator token from the manager's `harness-operator-token` file. `harnessctl login-url` prints the link. If the token file was deleted, a new one is made at the next start and old sessions stop working.

## `harnessctl` says it cannot reach the manager

It talks to `127.0.0.1:7421` by default, so run it on the manager's machine, or point it with `-api-addr`. It reads `harness-operator-token` from the current folder, or the file named by `-token-file`, or the `HARNESS_OPERATOR_TOKEN` environment variable.

## A Windows agent stops now and then

Task Scheduler's "restart on failure" covers only a launch that fails, so the tasks also run every two minutes to bring a stopped launcher back. If an agent stays down, check the task's last run result.

## Android

Hostnames may not resolve inside the phone app (give a relay as an IP address). Android may stop background processes under load; the app restarts the manager within seconds.

## Still stuck

Collect `harnessctl nodes`, the last lines of the manager's log (`manager.log` in its state folder on Windows; `journalctl -u home-harness-manager` on Linux) and `harnessctl audit`, and look at [Operations](operations.md).
