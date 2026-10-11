# Compared with others

Short version: Home Harness is for **a person or a small household** who wants their own mixed pile of computers to share work, with almost no setup and no trust assumptions beyond "my own network". The tools below are bigger, more mature and better at their own jobs. If one of them fits your problem, use it.

These notes describe the other projects as we understand them; they change, so check their own documentation before deciding.

## At a glance

| | Home Harness | Ray | Nomad | K3s / Kubernetes | Exo | BOINC |
|---|---|---|---|---|---|---|
| Main job | Spread typed tasks and files over home devices | Scale Python and ML code over a cluster | Schedule any workload (containers, binaries) over servers | Run containerized services | Run one large AI model across your devices | Volunteer computing for a science project |
| Typical scale | A few to a few dozen personal devices | Up to thousands of nodes | Up to thousands of nodes | Up to thousands of nodes | A handful of devices | Millions of volunteer machines |
| Runs on | Windows, macOS, Linux, Android | Mostly Linux (macOS and Windows partly) | Linux, macOS, Windows | Linux (Windows nodes possible) | macOS, Linux | Windows, macOS, Linux, Android |
| What you submit | A task from a fixed catalog, or a job or workflow of them | Python functions and actors | A job spec (Docker, exec, VMs) | Container images and manifests | A model to serve | Work units for a project's own app |
| Setup | An installer per device; approve by code | Cluster config; Python environments | Servers and a config file; ACL setup | Install and learn Kubernetes | Install on each device | Join a project in the client |
| Untrusted devices | Designed in (approval, spot checks), but not for hostile code | No: a trusted cluster | Trusted cluster | Trusted cluster | Trusted peers | Yes: that is its design |

## When to choose which

**Home Harness** when your fleet is laptops, desktops, a spare Mac, an old phone; you want to batch-process files, serve a local model from the best machine, and look at it on a phone; and you want every device to be approved by you. Strengths: tiny setup, runs on Windows, Mac, Linux and Android alike, typed tasks that behave the same everywhere, operator policy and an audit log, an OpenAI-compatible endpoint for your own models, one folder of state.

**Ray** when you are writing Python (or ML training and serving) that should scale from a laptop to a real cluster, with actors, object sharing and libraries such as Ray Data, Train and Serve. Home Harness has no programming model like that: it runs catalog tasks, not your arbitrary functions. It would be the wrong tool for distributed training.

**Nomad** when you want a general scheduler for containers, binaries and VMs on servers you run, with service discovery and the HashiCorp ecosystem, and operators who will maintain it. Home Harness is not a general scheduler, and Nomad has a far larger feature set (constraints, affinities, rolling deploys, multi-region).

**K3s / Kubernetes** when you run long-lived services in containers and want the whole Kubernetes ecosystem. Home Harness does not run your services; it runs finite tasks. K3s gives you a standard API, real multi-tenancy tooling and a large community, at the price of learning it and mostly Linux nodes.

**Exo** when your goal is to run one large AI model split across several of your own Apple or Linux machines, easily. Home Harness can split a model across devices too (a llama.cpp based "split session") and can serve many small models, but that is one feature among several; Exo is focused on it and is likely to be ahead there.

**BOINC** when you want to donate cycles to a research project, or to run your own volunteer-computing project with thousands of anonymous machines, with result validation by redundancy built in. Home Harness is for machines you own: it has optional spot checks, not a volunteer-grade validation system.

## Honest weaknesses of Home Harness

- **Young and small.** One manager (a standby copy can take over, but it is not a consensus cluster), no outside security audit, a small community, and few third-party integrations.
- **A fixed menu of tasks.** The catalog is the safety model and also the limit. Raw commands exist but are off by default, and containers are opt-in and need a container engine on the device.
- **Not for hostile code or multi-tenant use.** Devices are checked and tasks are confined, but there is no strong isolation (no VM or microVM sandbox), and it assumes you know whose machines they are.
- **Scale and performance are unproven beyond a home fleet.** It has not been run with hundreds of devices, and scheduling is simple (resource-aware, with priorities and retries), not a research-grade scheduler.
- **No GPU scheduling depth.** It reports GPUs and routes AI work by installed models, but does not manage GPU memory or time-slicing the way dedicated systems do.
- **Platform rough edges.** Windows laptops that sleep, Android background limits, unsigned binaries that SmartScreen warns about, and a few Mac and Linux paths that have had little hardware time.

If those are acceptable for your situation, Home Harness is meant to be the least effort way to put a home's spare compute to use.
