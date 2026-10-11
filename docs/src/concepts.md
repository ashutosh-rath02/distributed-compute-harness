# Concepts

## Manager and agents

The **manager** is the one coordinator. It keeps the list of devices, decides where work goes, serves the dashboard and the HTTP API, and stores files that tasks read and write. Its state (database, tokens, certificate) lives in one folder, so it is easy to back up and move.

An **agent** runs on each worker device. It connects *out* to the manager over a mutually authenticated TLS WebSocket, reports what the device has (CPU, memory, GPUs, which task types it can run, installed AI models), and runs what it is assigned. Windows, macOS, Linux and Android (through Termux or the app) are supported.

The manager never trusts an agent: it checks what an agent reports, and it never executes anything an agent sends. Agents never trust the manager blindly either: they pin its certificate fingerprint.

A device that is not on your home network can join through a **relay**, a small public server that only passes encrypted traffic between the two. It never sees your fleet's data in the clear.

## Joining and approval

A new device asks to join and shows a six-digit **pairing code**. The same code appears on the dashboard; if they match, you approve. A device that has joined once reconnects by itself. You can **revoke** a device, which makes the manager refuse it even with a valid token.

## Task types: the catalog

Instead of "run any program", the fleet offers a **catalog** of typed tasks: `image.resize`, `archive.zip`, `file.hash`, `llm.generate`, and so on. Each has a fixed list of parameters with limits, and is implemented inside the agent itself, so it behaves the same on every operating system and cannot reach outside its own working folder. See the [task reference](tasks.md).

Raw commands (`system.execute`) and host file reads exist but are **off by default**. You turn them on in the policy, per device label if you wish.

## Policy

The **policy** is your rule for what the fleet may run: which task types are switched on, on which devices (by label), with what maximum run time, and (for containers) which images. It applies to everything: the dashboard, the CLI, the API and the AI planner. Change it with `harnessctl policy` or `PUT /policy`.

## Workloads, jobs, workflows

- A **workload** is one task on one device. It is queued if every eligible device is full, and starts when one has room.
- A **job** is many tasks, spread over the fleet, with retries on a different device when one fails, and an optional **reduce** step that combines the results (for example, zip all the thumbnails). A job's progress counts completed, running, waiting and failed tasks.
- A **workflow** is steps that run in order, each on the results of the step before: resize every photo, then zip the results. Each step becomes an ordinary job when it is its turn.
- **Priority** (`high`, `normal`, `low`) decides who goes first when everything is busy; waiting work gains priority over time, and running work is never stopped.

## Files

Tasks read and write **files**. You upload a file to the manager (stored by its SHA-256, so identical bytes are stored once), name it as a task input, and download the task's output files afterwards. Stored files not needed by live work expire after a retention period.

## Plans: a job from plain words

You can describe what you want ("make thumbnails of these photos and zip them"). A local AI model proposes a **plan**: a catalog task or workflow with parameters. The plan is validated against the catalog and your policy and shown to you. **Nothing runs until you approve it**, and the AI key (see below) cannot approve.

## Credentials

- **Operator token**: full control of the manager. It lives in a file in the manager's state folder. The dashboard exchanges it for a session value kept in the browser; no cookies are used.
- **AI key**: opens only the OpenAI-compatible `/v1` routes, so a chat app can use your models without being able to run anything else.
- **Pairing token**: what a device presents to join without approval. Treat it like a password.
- **Remote key**: for optional remote dashboard access through the relay.

## Availability

Each device has an **availability rule**: by default it takes no new work while on battery. You can set it to always, only when idle, only when charging, paused, or limited to a daily time window. Running work always finishes.
