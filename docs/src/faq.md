# FAQ

## Does it need the internet?

No. Everything works on your local network. The internet is used only if you choose the relay (to reach devices away from home), to download an AI model, or to fetch container images.

## Do I need to open ports on my router?

No, not for devices on your home network. Only if you want devices outside to connect, and then through the relay, not directly to the manager.

## What happens if the manager goes off?

Agents keep trying to reconnect and work that was running there finishes or is retried; nothing new is placed until the manager returns. Its state is on disk, so a restart picks up where it left off. You can keep a **standby** manager that holds a copy and can be promoted (`harnessctl standby`).

## Is it safe to put a laptop I use every day in the pool?

By default a device takes no new work while on battery, agents run at low priority so your own apps come first, and you can set it to work only when idle, only when charging, or only at night (`harnessctl avail`). Tasks are sandboxed to their own folder. You can pause a device at any time.

## Can I run any program on my devices?

Raw commands exist but are **off by default**. The default is the typed catalog, which cannot run arbitrary programs. Turn raw commands on only for devices you label for it.

## Which operating systems work?

Windows, macOS, Linux (amd64 and arm64), and Android (a Termux agent, or the app, which can also host the manager). Some task types need tools on the device, such as ffmpeg for audio and video, or Ollama for AI models; the dashboard shows what each device offers.

## How do I use my own AI models?

Install Ollama on a device. Its models appear in the fleet (`harnessctl models`). Ask from the CLI (`harnessctl ask`), or point any OpenAI-compatible app at the base URL and key from `harnessctl ai`. Several devices can also share one large model (`harnessctl split`, using llama.cpp) when no single device can hold it.

## How do I update the agents?

The dashboard shows which devices run an older build and pushes the update (`harnessctl update <id>`). Agents from a package (apt, rpm, a launch agent from a `.pkg`) are updated by that package instead.

## How do I back up the manager?

Stop it, then `manager -export-state state.zip -state-dir <its state folder>`. The file holds your keys and tokens: keep it private. `-import-state` restores it on another machine, and devices that trusted the old manager keep trusting the new one.

## How do I remove it?

See [Uninstall](uninstall.md). Removal keeps your data unless you ask for it to be deleted.

## Is it production-ready?

It runs a real home fleet today, with tests behind it. It is young, built mostly by one team, has one manager (with an optional standby), and has not had an outside security audit. Judge it for what it is: a home and small-office tool. See [Compared with others](compare.md).

## Where is the code, and the license?

The repository is at https://github.com/ashutosh-rath02/distributed-compute-harness. Check the repository for the license terms.
