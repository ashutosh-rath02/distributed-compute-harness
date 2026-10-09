# Home Harness

Home Harness turns the spare computers and phones in your home into one pool. You install a **manager** on one machine and an **agent** on each of the others. Then you hand the manager work (resize these photos, hash these files, ask a local AI model a question), and it runs on whichever device has room.

Everything stays on your network. There is no cloud account, and no device is trusted by default: you approve each one, and the manager only ever asks them to run a fixed list of well-understood tasks unless you switch raw commands on.

## Where to start

- **[Quickstart](quickstart.md)**: install the manager, join a laptop, run a task. About five minutes.
- **[Concepts](concepts.md)**: the handful of ideas (manager, agent, task types, policy, jobs, workflows, plans) that everything else is built from.
- **[Task types](tasks.md)**: every task the fleet can run, with its parameters. Generated from the code, so it is never out of date.
- **[Client libraries](clients.md)** for Python and JavaScript, and the [HTTP API](api.md).
- **[Troubleshooting](troubleshooting.md)** for the things that actually go wrong: a join that does not appear, a firewall prompt, a laptop that went to sleep.
- **[Compared with others](compare.md)**: when to choose Home Harness, and when Ray, Nomad, K3s, Exo or BOINC fit better.

## What it is good for

- Batch work over many files, spread over every device you own: image resizing, archives, checksums, rendering, transcription.
- Running a local AI model on the best machine you have, and using it from your own apps through an OpenAI-compatible address.
- Keeping an eye on a small fleet from one dashboard, from your phone if you like.

## What it is not

It is not a cloud, a Kubernetes replacement or a way to run code you do not trust. See [Security](security.md) for exactly what it protects against and what it does not.
