# Home Compute Harness — Handover Document

**Status as of:** 2026-10-01 (sections 9, 10 and 11 updated after node revocation and the operator-API guard landed)
**Prepared for:** handover to another engineering agent (Codex) picking up this project
**Repo:** `home-harness` (Go module), GitHub `https://github.com/ashutosh-rath02/distributed-compute-harness.git`
**Companion docs already in this repo — read these too, this document does not replace them:**
- `v1.md` — the original v0 MVP plan, the frozen architectural principle (§2), and the full roadmap graphic (§22: v0→v1→v2→v3→v4→v5→v6→"Personal Distributed Compute Operating System").
- `Home_Compute_Harness_Baseline_Architecture.md` — the short baseline architecture reference (domain model table, extension points, design principles).

This document exists because those two describe the *plan*; this one describes *what actually got built*, *why specific decisions were made the way they were*, *what's known to be broken or deferred*, and *what a sensible next increment looks like*. Where this document and the code disagree, trust the code — this is a snapshot, not a live source of truth.

---

## 1. What this project is

A personal, local-first distributed compute harness: a way to discover, register, and use spare compute capacity across a person's own devices (laptops, an Android phone today; more device types later) without depending on any cloud provider or third-party service for the core control plane. The guiding roadmap (`v1.md` §22):

```
v0  Connectivity + Identity + Resources + Communication
v1  Actual workload execution
v2  Scheduling + resource-aware placement
v3  Services + desired-state reconciliation
v4  Phones + edge devices + capability invocation
v5  Mesh networking + remote devices
v6  AI intent planner
    → Personal Distributed Compute Operating System
```

**v0 through v5 (LAN onboarding part 1/1b/2, and remote part 1) are done.** See §9 for the precise current state and §10 for what's next.

---

## 2. The one rule that must never be violated

From `v1.md` §2, restated because it has shaped every design decision in this codebase:

> **The Harness Core must not depend on a specific operating system, device type, transport mechanism, runtime, or future capability.**

Concretely: `internal/domain` defines interfaces (`Transport`, `Conn`, `Discoverer`) and plain data types (`Node`, `Workload`, `Resource`, `Capability`, `Event`...). Nothing in `internal/manager` or `internal/agent` imports a concrete transport package by name — they take a `domain.Transport` value injected at construction time. Every new capability (a new transport, a new onboarding path, a new capability type) should enter as an *implementation of an existing interface* or a *new case in an already-generic dispatch*, not as a new special case threaded through the core. When in doubt, check whether the frozen principle is at risk before writing code — it has been the single most load-bearing constraint across five-plus increments of this project, and it is why the relay feature (§8.4) could be added as a new package with zero changes to `manager.Server`'s core loop.

---

## 3. Repository map

```
cmd/
  manager/    — control-plane binary (composition root: builds transports, opens store, starts Server)
  agent/      — node binary (composition root: builds transport, starts Agent)
  relay/      — standalone rendezvous relay binary (v5 remote part 1)
  harnessctl/ — CLI for operators (nodes, run, join, events, workloads...)

internal/
  domain/         — core types + interfaces; the only package everything else may depend on freely
  protocol/        — wire envelope + message payload types (JSON over whatever Transport carries)
  identity/        — Ed25519 node identity, persisted keypair, NodeID derivation
  mtls/            — manager TLS certificate (generate/persist/fingerprint), pinned-fingerprint client config
  discovery/udp/   — LAN multicast discovery beacon (manager side) + discoverer (agent side)
  transport/ws/    — WebSocket-over-TCP domain.Transport implementation (the original, LAN-capable transport)
  transport/relay/ — domain.Transport implementation tunneled through a relay (v5 remote part 1)
  transport/multi/ — fan-in composite domain.Transport (manager listens on LAN + relay at once)
  relay/           — the relay's own rendezvous protocol + server (used by cmd/relay and transport/relay)
  joinscript/      — shared onboarding-script generator (used by harnessctl join and the dashboard endpoint)
  eventbus/        — pub/sub for domain.Event (used by SSE /events and future consumers)
  artifacts/       — the manager's content-addressed file store for workload inputs/outputs (roadmap item 5)
  instancelock/    — one live agent process per identity directory (flock / LockFileEx)
  store/persistent/ — bbolt-backed persistence for node manifests + workload records
  sysinfo/         — live resource sampling (CPU%, memory) via gopsutil
  agent/           — Agent type: connect/register/heartbeat/reconnect loop, command + workload execution, self-update
  manager/         — Server type: registry, placement, workload lifecycle, reconciliation, HTTP API, dashboard

test/integration/  — full manager+agent(+relay) black-box tests over real transports, not mocks
```

Every `internal/*` package other than `domain` is intentionally narrow — most are under 200 lines. This is deliberate: the project favors many small, independently-testable packages over few large ones.

**Key third-party dependencies** (`go.mod`, Go 1.27): `nhooyr.io/websocket` (the WebSocket implementation both `transport/ws` and `transport/relay` build on), `go.etcd.io/bbolt` (embedded KV store behind `internal/store/persistent`), `github.com/shirou/gopsutil/v4` (cross-platform CPU/memory sampling behind `internal/sysinfo`). Everything else is standard library — this is intentional; the project avoids adding dependencies where the standard library suffices (e.g. `crypto/tls`, `debug/pe`/`debug/elf` for binary platform detection, `encoding/json` for the wire protocol).

---

## 4. Core domain model (`internal/domain`)

This is the vocabulary every other package speaks. Read this section before touching manager or agent internals.

### Node identity and description
- **`Node`** (`node.go`): stable identity/metadata only. `Identity{NodeID, PublicKey}` (Ed25519), `Hostname`, `Name`, `Platform{OS, Architecture}`, `AgentVersion`, `BinaryHash` (SHA-256 of the running agent binary — this is what self-update compares against the manager's served binary).
- **`NodeState`** enum: `UNKNOWN | DISCOVERED | REGISTERING | CONNECTED | READY | DEGRADED | OFFLINE | RECONNECTING`.
- **`Manifest`** (`manifest.go`): what a node publishes on registration — `SchemaVersion`, `Node`, `[]Resource`, `[]Capability`.
- **`Resource`** (`resource.go`): `Kind` (open string — `cpu.cores`, `memory.bytes`, `storage.bytes` today), `Capacity float64`, `Unit`. A *static* declared quantity.
- **`Capability`** (`capability.go`): `Name` (open string — `system.execute` and `filesystem.read` implemented, `filesystem.write` reserved/unimplemented) + `Version`.
- **`RuntimeState`** (`runtime_state.go`): the *fast-changing* half of node state, deliberately kept separate from `Node`/`Manifest` — `NodeID, State, CPUPercent, MemoryAvailableBytes, Uptime, LastHeartbeat, ConnectedAt, AgentVersion`. Never persisted alongside identity.

### Workloads
- **`Workload`** (`workload.go`): `ID`, `Target NodeID`, `Command`/`Args []string` (argv form — **no shell interpretation**, this is deliberate for both correctness and security), `Pinned bool` (was this workload explicitly targeted at a node, vs. auto-placed? — preserved across restarts so a restart of an auto-placed workload can re-run placement, but a pinned one stays put), `Requirements ResourceRequirements`, `RestartPolicy` (immutable once submitted), `Capability CapabilityName` (empty resolves via `EffectiveCapability()` to `system.execute`), `Params map[string]string`.
- **`WorkloadState`**: `QUEUED | PENDING | RUNNING | COMPLETED | FAILED | CANCELED | UNKNOWN` (`QUEUED` = accepted, waiting for a node with a free slot — see §7 "Capacity reservation and the queue"; `UNKNOWN` = status couldn't be resolved after a manager restart — an orphaned in-flight workload).
- **`WorkloadStatus`**: `Stdout/Stderr` (capped at 64KB, `Truncated` flag), `ExitCode`, `Error`, `StartedAt`, `FinishedAt`; for the queue, `QueuedAt` (FIFO order, kept across re-queues), `NotBefore` (retry backoff), and `Retryable` (set by an agent only on a pre-start "all slots busy" refusal).
- **Workload files** (`artifact.go`): `Workload.Inputs []ArtifactRef{Name, SHA256, Size}` and `Workload.Outputs []string` (declared), `WorkloadStatus.Outputs []ArtifactRef` (delivered, as the manager verified them). `ValidArtifactName` is one rule on every OS: forward-slash relative path, ≤4 segments of `[A-Za-z0-9._-]{1,64}`, no `.`/`..`, no trailing dot, no Windows device names (`con`, `nul.txt`, ...). `ValidateWorkloadFiles` also forbids two names landing on one file — compared case-insensitively — and a name that is a directory of another (`a` vs `a/b`). ≤16 inputs, ≤16 outputs, `system.execute` only. Agents advertise `FeatureArtifacts` (`artifacts.v1`).
- **Batch jobs** (`job.go`): `Job{Tasks []TaskSpec, Reduce *TaskSpec, MaxAttempts, State}` — `TaskSpec` is what one workload submission carries. Each attempt is an ordinary workload with `Workload.Job`/`Task` (key `"0007"` or `"reduce"`)/`Attempt` and `AvoidNodes`. `WorkloadStatus.NodeLost` marks a FAILED the manager wrote because the node went away. ≤1000 tasks; `MaxWorkloadInputs` is 256 so a reduce can take many parts.
- **`Manifest.WorkloadSlots`** (`manifest.go`): how many workloads the agent runs at once; `Slots()` treats absent/0 as 1, which is exactly how agents that predate it behave.
- **`PersistedWorkload`** = `Workload + Status + RestartState` — the on-disk survival unit; always written together so a restart never sees a status without its matching restart bookkeeping.
- **`ResourceRequirements`** (`requirements.go`): `MinCPUCores` (checked against *static* declared capacity), `MinMemoryBytes` (checked against *live* `RuntimeState.MemoryAvailableBytes`), `MaxCPUPercent` (checked against live CPU%). All zero = no constraint.
- **`RestartPolicy`** (`restart_policy.go`): `Never (="") | OnFailure | Always`. `WantsRestartAfter(state)`: `COMPLETED` restarts only under `Always`; `FAILED`/`UNKNOWN` restart under `OnFailure` or `Always`; `PENDING`/`RUNNING` never restart (they're not terminal). `RestartState{Count, BackoffCount, NextRestartAt}` — `Count` is lifetime/display-only, `BackoffCount` drives the actual backoff curve and resets to 0 once a run has stayed healthy past a threshold (5 minutes, see §8.2).

### Commands and events
- **`Command`**: stateless request/reply, not a workload. `CommandName`: `PING | ECHO | GET_SYSTEM_INFO | GET_AGENT_STATUS | REQUEST_RESOURCE_REFRESH | SELF_UPDATE`. `CommandResult{Success, Output map[string]string, Error}`.
- **`Event`**: `{Type, NodeID, Timestamp, Data}` — `node.*` / `command.*` / `workload.{assigned,started,completed,failed,canceled}` types, published to `internal/eventbus` and exposed over the manager's `GET /events` SSE stream.

### Interfaces
- **`Transport`** (`transport.go`) — `Dial(ctx, addr) (Conn, error)`, `Listen(ctx, addr) (<-chan Conn, error)`. The one seam every concrete carrier (`ws`, `relay`) implements.
- **`Conn`** — `Send/Receive([]byte)`, `RemoteAddr() string`, `Close() error`. Messages are opaque framed bytes; encoding is `internal/protocol`'s concern, not the transport's.
- **`Discoverer`** (`discovery.go`) — single method `Discover(ctx) (managerAddr string, err error)`. LAN multicast today (`internal/discovery/udp`); the interface says nothing about *how*.

---

## 5. Wire protocol (`internal/protocol`)

Every message is a self-contained JSON **Envelope** with its own `Encode`/`Decode`, carrying a `Type`, `Source`/`Dest` NodeIDs, and a payload. One envelope = one `Conn.Send`/`Receive` call = one WebSocket binary frame (for the `ws` transport) — there is no additional length-prefixing needed because the underlying transport already delimits messages. This matters if you ever add a transport that doesn't naturally preserve message boundaries (e.g. a raw TCP stream): it would need its own framing, since `protocol.Envelope` assumes none.

Message types include `REGISTER`/`REGISTER_ACK`/`REGISTER_REJECT`, `HEARTBEAT`, `PING`/`PONG`, `COMMAND`/`COMMAND_RESULT`, `WORKLOAD_ASSIGN`/`WORKLOAD_STATUS`/`WORKLOAD_CANCEL`, `CAPABILITY_UPDATE`, `ERROR`.

**Registration/authentication model** (important, and easy to describe wrong): this is **not** classic mutual TLS. The manager's TLS is server-only, with the client (agent) pinning the server's certificate fingerprint by trust-on-first-use (`internal/mtls`, see §6). Per-node authentication happens **one layer up**, inside the already-encrypted connection: each node has a persistent Ed25519 keypair (`internal/identity`), its `NodeID` is derived from the public key, and `REGISTER`'s payload carries a signature over the registration data proving possession of the private key. A shared `PairingToken` (an operator-set bearer secret, not per-node) gates registration entirely separately. So: TLS proves "this is the manager I expect", the signature proves "this connection really is the node it claims to be", and the pairing token proves "this node is authorized to join at all".

---

## 6. Security building blocks (`internal/mtls`, `internal/identity`)

- **`internal/mtls`**: generates and persists a self-signed **ECDSA P-256** certificate for the manager (`LoadOrCreateCert`), computes its SHA-256 fingerprint (`Fingerprint`), and builds a client `tls.Config` that skips normal chain validation and instead pins that exact fingerprint (`PinnedClientConfig`). **Do not change this to Ed25519** — see §11's gotcha list; this was a real, hardware-discovered bug (Windows schannel cannot handshake against an Ed25519 cert at all, no client-side workaround exists).
- **`internal/identity`**: Ed25519 keypair generated on first run and persisted under the agent's identity directory; `NodeID` is deterministically derived from the public key, so a node's identity — and its place in the manager's registry — survives reinstalls as long as the identity directory is preserved.

---

## 7. Manager internals (`internal/manager`)

### Registry (`registry.go`)
In-memory `map[NodeID]*NodeRecord`, each holding `Node`, `Resources`, `Capabilities`, `State`, `LastSeen`, `Conn`, `LastMetrics RuntimeState`. Notable behaviors:
- `Upsert` clears `LastMetrics` on reconnect — a reconnecting node's *stale* last-known metrics must not be trusted until it heartbeats again.
- `SetOfflineIfCurrent` guards against a race where a stale connection's own cleanup could clobber a *newer* reconnection's state.
- `Seed` restores previously-known (OFFLINE) nodes from the persistent store after a manager restart, without disturbing nodes that are already live.
- `ExpireStale(timeout)` is what the heartbeat-monitor ticker calls to mark nodes OFFLINE.

### Placement (`placement.go`) — v2's resource-aware scheduling
`nodeFits(rec, capability, requirements)`: capability match is checked unconditionally; if requirements need *live* metrics (memory/CPU%) and the node has never heartbeated, it's ineligible. `selectNodeWithUsage` filters via `nodeFits` and the node's free capacity (below), then picks the candidate with the **most memory left after reservations**, then the fewest running workloads, then ascending NodeID — deterministic, and a burst now spreads across nodes instead of piling onto whichever had the most free memory a heartbeat ago. Still deliberately simple, not a multi-factor scorer.

### Capacity reservation and the queue (`queue.go`) — roadmap item 4
- **Slots.** Each node runs up to its advertised `WorkloadSlots` at once (agent `-slots`, default half the logical CPUs, 1–16; absent in an old manifest = 1). Same-machine identities (§9) are *not* pooled: the host fingerprint is an untrusted hint.
- **Reservations are derived, never counted.** `usageOn(node)` sums every PENDING/RUNNING workload targeting the node (count, `MinCPUCores`, `MinMemoryBytes`) from the workload registry each time, so every exit path — completion, cancel, node loss, revocation, restart, a manager restart turning in-flight work UNKNOWN — frees capacity with no bookkeeping to drift. `hasRoom` then checks slots, declared cores minus reserved cores, and live free memory against reserved + requested (conservative: may under-place, never over-place).
- **Queue, not rejection.** A submission some ready node *could* run but none has room for right now is stored `QUEUED` (persisted; survives a manager restart unchanged) and `POST /workloads` returns it with `"state":"QUEUED"`. Submissions no ready node could ever fit, or with no ready node at all, are still rejected (409) — queueing those would park work nowhere. "Could ever fit" (`couldEverFit`) looks only at *declared* capacity: capability, cores, total `memory.bytes`. Live free memory, CPU load, and "hasn't heartbeated yet" reflect what is running now, so failing them queues rather than rejects (a node declaring no total memory falls back to its live figure). Caveat: a request close to a node's total memory can wait a long time, since the OS never frees all of it. A queued workload waits indefinitely (its pinned node may come back); `cancel` drops it at once, and revoking a node cancels work pinned to it.
- **Dispatcher.** `dispatchQueued` runs oldest-first (`QueuedAt`, then ID) on every reconcile tick and whenever a slot frees (`kickDispatch` on any terminal status and after a node registers). No head-of-line blocking: a smaller later workload may start ahead of an older one that still fits nowhere. `placeMu` serializes every resolve-then-reserve (submit, dispatch, restart) so two placements can't both take a node's last slot.
- **Busy refusals.** If an agent still finds itself full (its view raced the manager's), it reports FAILED with `Retryable` and no `StartedAt`; the manager re-queues that workload (keeping its `QueuedAt`, clearing an unpinned target, backing off 1s→30s via `NotBefore`) and holds new work off that node for `busyHoldOff` (5s) or until it reports a finished workload. The hold-off is a timeout on purpose: the refusal can cross that node's own completion report on the wire, and a flag cleared only "when it finishes something" would strand an idle node forever. Only a PENDING workload can be re-queued (a stale or forged refusal can't resurrect finished or running work), and an operator cancel that crosses a refusal wins (`cancelRequested`).
- Restarts (`restartWorkload`) that find every eligible node full are deferred to the next tick like any other no-room-right-now restart; they don't enter the queue.

### Workload files (`artifacts.go`, `internal/artifacts`) — roadmap item 5
- **Store.** `internal/artifacts` keeps files at `<dir>/sha256/<aa>/<sha>`, streaming each write through SHA-256 into `<dir>/tmp` and renaming only after the per-file (`-artifact-max-size`, default 256 MiB) and whole-store (`-artifact-store-size`, 4 GiB) limits and any expected hash check out; nothing partial is ever kept, identical content is stored once. The Termux launcher keeps it in `state/artifacts`; a store that fails to open logs `WORKLOAD FILES DISABLED` and the manager runs on without the feature (like a bad `-agent-binary` set), never crash-looping a boot launcher. GC (hourly and at startup) deletes files unused for `-artifact-retention` (7 days, by mtime) unless a *live* workload — QUEUED/PENDING/RUNNING/UNKNOWN or due a restart — names it as an input; submit and every dispatch `Touch` inputs so GC can't race them. `-artifact-dir ""` disables workload files.
- **Operator side.** `POST /artifacts` (raw body, optional `X-Artifact-SHA256`), `GET /artifacts`, `GET /artifacts/{sha}`, `DELETE /artifacts/{sha}` (409 while live). `POST /workloads` takes `inputs:[{name,sha256}]` and `outputs:[names]` (400 for bad names, unknown inputs, or files on a non-`system.execute` capability). Downloads are always `application/octet-stream`, `attachment`, `nosniff`, `CSP: sandbox`: the bytes may come from an agent and this origin holds the operator credential. The dashboard saves them only through an `<a download>` of an octet-stream Blob — never opening or navigating to a `blob:` URL, which would inherit the page's origin.
- **Agent side, per assignment.** Every `WORKLOAD_ASSIGN` of a workload with files (submit, queue dispatch, restart — all through `assign()`) mints a fresh 256-bit token sent in `WorkloadAssignPayload.ArtifactToken` — deliberately not on `domain.Workload`, so it is never persisted, logged, or shown by the API. On the agent transports (`/workload-artifacts/`, registered next to `/agent-binaries/` in `cmd/manager`, so direct TLS and the relay both carry it), `GET .../{workload}/inputs/{sha}` serves only that workload's declared inputs and `PUT .../{workload}/outputs/{name}` accepts only its declared outputs, each **once per attempt**, hash-verified, with a `Content-Length` reserved up front against a per-attempt budget (all outputs together may use at most `-artifact-max-size`), so one token can't fill the store. A token is good only while its workload is PENDING/RUNNING on that node; it is dropped on any terminal report and on a busy refusal, and swept every reconcile tick otherwise (node lost, revoked, canceled). Tokens are looked up by their hash.
- **Settlement.** On a terminal report the agent's claimed outputs are replaced by what the manager actually received for that attempt (name + sha + size must match); a COMPLETED report missing any declared output is recorded as FAILED. Placement only considers agents advertising `artifacts.v1` for a workload with files (an older agent would ignore the fields and run the command without them).

### Batch jobs (`jobs.go`, `jobs_api.go`) — roadmap item 6
- **Derived, not stored.** The job record holds only the request and the final outcome. Each pass, `byJob()` groups every attempt (one registry scan) and `deriveTask` reads a task's state from its attempts: the current attempt is the highest `Attempt`; FAILED attempts count against `MaxAttempts` (the first run included) and their nodes go into the next attempt's `AvoidNodes`; attempts lost to the node going away (`NodeLost`) or to a manager restart (`UNKNOWN`) don't count and don't avoid the node — up to `maxNodeLostRetries` (5), after which they count, so a task that keeps killing its device can't retry forever. A crash between "submitted an attempt" and "updated the job" therefore can't lose or duplicate a retry.
- **One submitter.** `advanceJobs` runs in the reconcile goroutine (every tick and every dispatch kick, which every terminal status fires) and is the only code that submits attempts; `SubmitJob` validates, persists (`jobs` bolt bucket), and kicks it. When every task COMPLETED, the reduce is submitted once with each task's outputs at `parts/<task-key>/<name>` plus its own inputs; its outputs are the job's result. A task out of attempts (or canceled) makes the job FAILED once nothing is left running — the other tasks still finish and keep their outputs; the reduce never runs.
- **Refused up front.** Tasks without a command, files that fail validation, a task or reduce no ready node could ever run, and a reduce whose parts wouldn't fit (`ValidateWorkloadFiles` over the would-be `parts/` names: count ≤256, ≤4 segments, so map outputs may have at most 2) are rejected at submit, not after the work ran.
- **Cancel** marks the job CANCELED first (nothing retries), cancels every in-flight attempt, and a sweep on the next pass cancels any attempt submitted concurrently. Operator-canceling one attempt ends that task (the job becomes FAILED).
- **Manager restarts are not failures.** `handleConn` no longer fails a node's in-flight work when the *manager's* context is done: it stays PENDING/RUNNING on disk and is seeded UNKNOWN, so a graceful restart costs a job nothing. Real node loss goes through `FailInFlightFor`, which sets `NodeLost`.
- **GC** keeps every running job's task and reduce inputs and its finished tasks' outputs (the reduce's future inputs) live.
- **Queue fairness:** FIFO, so a single workload submitted after a 1000-task job waits behind it. Fine for v1.
- API: `POST /jobs` (8 MiB body), `GET /jobs`, `GET /jobs/{id}` (per-task state/attempts/node/outputs/error/waiting reason), `POST /jobs/{id}/cancel`. CLI: `harnessctl map -each FILE|GLOB ... | -count N [-shared F] [-out N] [-attempts N] [-reduce "cmd args" -reduce-out N] <cmd> {in}/{i}`, `jobs`, `job`, `job-outputs`, `job-cancel`. The dashboard's files card has "Run once per input file" (+ shared files) and a Jobs section with progress, per-task downloads, and Cancel.

### Placement and the dispatch pass (item 6 refactor)
`resolve(placement{target, capability, req, features, avoid}, usage)` is the one placement function (`resolveWorkloadTarget` remains a thin wrapper). Avoided nodes are dropped if any other node could ever run the work — a retry then waits for those rather than going back — and used anyway if only they could. `dispatchQueued` holds `placeMu` for a whole pass, places against one usage snapshot it updates as it assigns (one registry scan per pass instead of one per workload per node), skips the remaining *unconstrained* workloads of a capability once one finds no room, and persists/sends only after unlocking.

### Workload lifecycle + reconciliation (`workloads.go`, `reconcile.go`) — v3's services model
`WorkloadRegistry` tracks `WorkloadRecord{Workload, Status, Restart}` keyed by `WorkloadID`. `FailInFlightFor(nodeID)` fails any PENDING/RUNNING workload when its node drops. `RestartCandidates(now)` filters by `RestartPolicy.WantsRestartAfter` plus an elapsed `NextRestartAt`. `MarkRestarting` resets status to PENDING, clears prior output, bumps the lifetime `Count`, and resets or increments `BackoffCount` depending on whether the previous run was "healthy" (lasted ≥ 5 minutes, `restartHealthyRunThreshold`). `DeferRestart` bumps `NextRestartAt` *without* counting it as a real restart attempt — used for benign placement failures (e.g. no eligible node right now) so those don't chew through the backoff budget.

Reconciliation runs on its own ticker (`ReconcileInterval`, default 5s — separate from heartbeat monitoring). Each tick: find restart candidates → resolve target (pinned workloads stay on their node; unpinned ones re-run placement) → on success, `MarkRestarting` + re-dispatch `WORKLOAD_ASSIGN`; either way, `DeferRestart` schedules the next check using exponential backoff (`backoffFor`: base 5s, doubling per attempt, capped at 5 minutes, with an overflow guard).

### HTTP API and dashboard (`api.go`, `dashboard.go`, `join.go`, `binaryplatform.go`, `selfupdate.go`)
The manager runs **two separate HTTP surfaces**:
1. The agent-facing transport (`:7420` by default) — where `domain.Transport.Listen` actually accepts node connections. Agent-binary downloads are registered on *this* listener (and the relay-backed one) via `Transport.Handle`, deliberately reusing the exact port agents already dial rather than opening a new one: `GET /agent-binaries/{os}/{arch}` serves each platform's build, and the legacy `/agent-binary` serves the catalog's *primary* build for agents that predate it.

**Agent catalog** (`agentcatalog.go`): `-agent-binary` is repeatable, one build per platform, platform read from the PE/ELF/Mach-O header (or given as `os/arch=path`); conflicts, duplicates, and undetectable files stop the manager at startup. There is deliberately **no wildcard build**: a node is only ever compared with, offered, or onboarded with its own platform's build. (A platform-blind comparison once meant `harnessctl update <android-node>` against a Windows `agent.exe` would install the Windows binary on the phone — its hash check passing, since the manager named that file — and brick the agent.) The server computes each node's `updateStatus` — `current`, `available`, `reinstall-required`, `unknown` — and the dashboard, `harnessctl nodes`, and `POST /nodes/{id}/update` all use it. Mixed-version fleets are handled through `domain.Manifest.AgentFeatures`: agents advertising `self-update.path` follow the SELF_UPDATE command's per-platform `path`; older agents can only fetch `/agent-binary`, so they're updated only when the primary (windows/amd64 whenever loaded, never flag order) is their own platform, and get `reinstall-required` otherwise. Item 4's slot count should use the same mechanism (absent = legacy behavior). Every download is logged with its route. `manager -check-agent-binaries -agent-binary ...` validates a set and exits; the Termux installer runs it with the new manager *before* stopping the running one, so a bad stage (classically an old `agent.exe` left next to `agent-windows-amd64.exe` — two windows/amd64 builds) is refused instead of leaving Termux:Boot relaunching a manager that can't start.
2. The operator-facing HTTP API (`127.0.0.1:7421` by default, **loopback-only** — this is a hard trust-model line, see §11) — `GET /nodes`, `GET /nodes/{id}`, `GET /events` (SSE), `POST /nodes/{id}/commands`, `POST /nodes/{id}/update`, `GET /agent-binaries` (the catalog), `GET /agent-binary/hash` (legacy primary only), `GET /join-info` (lists `agentBinaries`; the old single-hash fields were removed on purpose so an older CLI can't build an Android script around a Windows hash), `GET /join-script`, `GET /` (the dashboard), `POST /workloads` and friends.

`GET /` serves a single self-contained embedded HTML/CSS/JS file (`dashboard.go`'s `//go:embed dashboard.html`) — no build step, no CDN dependency (this project is local-first by design, so the dashboard must work with zero external network reachability). It lists nodes (live via the SSE stream, no polling loop), shows a platform-aware "(outdated)" marker (see `binaryplatform.go` — detects the served binary's OS/arch from its PE/ELF header, so a Linux node's hash is never wrongly compared against a Windows binary's hash), and has an "Add a device" form that calls `GET /join-script` to generate a copy-pasteable onboarding script.

`internal/joinscript` is the **single source of truth** for onboarding-script text (PowerShell for Windows, bash for Android/Termux), used identically by `harnessctl join` and the dashboard's endpoint — this avoids the two ever silently drifting apart.

---

## 8. Agent internals (`internal/agent`)

### Connection lifecycle (`agent.go`)
`Agent.Run(ctx)` loops `connectAndServe` with **exponential backoff**: starts at `Config.ReconnectBackoff`, doubles on each failure up to `MaxReconnectBackoff`, and resets to the starting value once a connection has stayed up longer than `3 × HeartbeatInterval` (a "real session," not a flash-in-the-pan reconnect). `resolveManagerAddr`: if `Config.ManagerAddr` is set, use it directly and never consult `Discoverer`; otherwise ask `Discoverer.Discover(ctx)` on every (re)connect attempt (not just once — the manager's address may change across restarts).

### Command and workload execution (`commands.go`, `workloads.go`, `executor.go`)
Two distinct paths:
- **Commands** are stateless request/reply (`handleCommand`/`executeCommand` in `commands.go`) — `PING`, `ECHO`, `GET_SYSTEM_INFO`, etc. `SELF_UPDATE` is special-cased to **ack synchronously before** `performSelfUpdate` runs (so the ack isn't lost when the process replaces itself).
- **Workloads** are longer-running (`handleWorkloadAssign` in `workloads.go` → `Executor.Start`, which dispatches to `startExecute` (`system.execute`) or `startFilesystemRead` per the workload's effective capability). The executor streams status back (`RUNNING` as soon as the subprocess starts, then a terminal state) rather than blocking until completion. It runs up to `Slots()` workloads at once (`-slots`, advertised in the manifest); one more is refused with `ErrExecutorFull` (reported as a retryable refusal — queueing is the manager's job), and a duplicate of an ID already running is refused outright. A slot is released *before* the terminal status is sent, so the manager never dispatches into a slot the agent still holds. `CancelCurrent` (connection loss) cancels every running workload.
- `InsecureWorkloadsDisabled`: an agent run with its own `-insecure` refuses to execute workloads at all, even if a manager (which it can't verify) tries to assign one — this is a deliberate refusal to run arbitrary code for an unauthenticated peer.

### Workload files (`files.go`, `artifacts.go`)
A workload that declares files runs in its own goroutine: a fresh working directory `<work-root>/<workload-id>` (`-work-dir`, default the user cache dir + node ID — **never overlapping the identity dir**, which holds the node key; `New` refuses either containing the other), inputs fetched and verified (size + SHA-256, written to `.part` and renamed) before the process starts with that directory as its cwd, then — only after a successful exit — each declared output must be a regular file (`Lstat`, no symlinks) and is uploaded; a missing one FAILS the workload (strict, so batch jobs can rely on outputs). Canceled or failed runs upload nothing. The slot stays held through the transfers, and the directory is removed *before* the terminal status is reported. `Run` (not `New`, which a standby copy also calls before the instance lock) clears leftover directories, touching only names shaped like workload IDs. Transfers reach the manager exactly like self-update (direct pinned TLS or the relay's HTTP client) but with no 2-minute total timeout; the workload's own context bounds them. Workloads without files are untouched: no working directory, same cwd as before.

### Self-update (`selfupdate.go`)
Downloads a new binary over a direct or relay-mediated HTTP(S) connection, verifies its SHA-256 against the expected hash, sets it executable (`0o755` — **do not regress this to `os.Create`'s default mode**, see §11), and relaunches with the exact flags the process was originally started with (`os.Args[1:]`, captured as `LaunchArgs`). Relay downloads use an injected HTTP client so the agent core does not depend on the concrete relay transport.

---

## 9. Onboarding and connectivity paths — current state

There are now three distinct ways a node joins a manager, in the order they were built:

1. **LAN, manual `-manager-addr`/`-manager-fingerprint` flags** (v0/v1 era) — always available, most explicit.
2. **LAN, UDP multicast discovery** (`internal/discovery/udp`) — zero-config *if* the device is on the same broadcast domain; genuinely LAN-scoped (multicast TTL=1), cannot help a remote device.
3. **LAN, one-time `harnessctl join`/dashboard-generated script** (v5 part 1/1b/2) — operator runs a generated PowerShell (Windows) or bash (Android/Termux) script once; handles TLS fingerprint + pairing token and installs an automatic launcher. Windows uses the current user's `HKCU\...\Run` entry plus a hidden retry loop; Android uses Termux:Boot and a wake lock. LAN launchers deliberately omit `-manager-addr`, so the existing multicast discoverer follows a phone whose Wi-Fi address changes. `internal/joinscript` is the shared script-generation logic; the manager's local **web dashboard** (`GET /`, loopback-only) is the friendliest way to generate one, reusing `GET /nodes` + `GET /events` (SSE) + `GET /join-script`.
4. **Off-LAN, relay-based** (v5 remote part 1, most recent) — for a device that isn't on the manager's LAN at all (different network, mobile data). See §9.1.
6. **macOS and Linux desktops** (`joinscript/unix.go`): the same QR/link and `harnessctl join <addr> macos|linux` flows produce a bash installer that detects the CPU (`uname -m`) and downloads that architecture's build — the manager hands Unix scripts *every* build for the OS, since a Mac or Linux box's CPU isn't implied by its platform — verifies its hash (`sha256sum`/`shasum`), and installs persistence: a launchd user agent on macOS, a `systemd --user` unit on Linux (cron `@reboot` + nohup fallback). Invitation pages show a `curl -fsS[k] '<invite>/setup' | bash` one-liner so zsh never parses a pasted script. Verified by running the real generated Linux invitation script under Git Bash against a real manager (download, hash check, install, launcher, tamper refusal); launchd itself needs a real Mac.
5. **Phone-first QR/link enrollment** — the loopback dashboard creates a 10-minute, single-use invitation for Windows or Android. LAN invitations point at the phone directly; internet invitations use the relay's browser-trusted HTTPS gateway. The QR/link contains no permanent pairing token or manager relay session. A relay enrollment receives an opaque, authenticated per-device relay credential that remains usable across relay restarts when `-alias-key` is preserved.

### Admission vs. reconnect, and revocation

Admission and reconnection are different credentials. A previously unknown identity needs an admission credential at `REGISTER` (the shared `-pairing-token`, or a consumed one-time invitation token); a known identity reconnects on proof-of-possession of its Ed25519 key alone (`handleRegister`). Because of that, rotating the pairing token no longer evicts anyone, so **revocation** (`internal/manager/revocation.go`) is the way to take an admission back: `POST /nodes/{id}/revoke`, `harnessctl revoke <id>`, or the dashboard's per-node *Revoke* button. A revocation is a persisted denylist entry (bolt `revoked` bucket, written in the same transaction that forgets the node's record) checked in `handleRegister` *before* any token is considered, under `admitMu` so a racing `REGISTER` can't re-add a node mid-revoke. Revoking closes the live connection (the agent cancels its running workload on disconnect), fails in-flight unpinned work (its restart policy may re-place it elsewhere), and cancels workloads pinned to the node (otherwise the reconciler would defer them forever). `DELETE /revocations/{id}` / `harnessctl unrevoke` lifts it; the node then needs a fresh admission (a shared-token launcher does this on its own; an invitation-enrolled node needs a new invitation). Limits, stated wherever revocation is offered: it denies one *identity* (a shared-token holder can mint a new one, so also rotate `-pairing-token`), and an `ha1.` relay alias stays usable at the relay until `-alias-key` is rotated (the manager still rejects it at `REGISTER`). A revoked agent keeps retrying at its normal capped backoff (30s), each attempt logged as `registration rejected: node revoked by operator`.

### Fleet hygiene: same-machine hints, operator names/labels, audit log

- **Host fingerprint** (`internal/sysinfo/hostid.go`): agents report `hex(SHA-256("home-harness-host-v1:" + machine ID))[:32]` plus a source — `machine` (Windows MachineGuid, Linux product_uuid/machine-id) or `boot` (Termux usually has only `boot_id`, so a phone's value changes across reboots). It is an **untrusted, agent-asserted hint**: cloned VMs/images share machine IDs and a hostile agent can copy one. `fleet.go` therefore groups identities by fingerprint but only *collapses* a group (counts its capacity once in `/resources/total` and the dashboard summary) when declared cores and memory also match; a fingerprint match with different hardware is a `hostConflict` and both count. Nothing automatic ever acts on it — **item 4 must not enforce reservations on it without that corroboration**. Dashboard wording is deliberately neutral ("reports the same machine as …", with last-seen times). Tests on one PC simulate machines with agent `Config.HostFingerprint` (empty = detect, `-` = none).
- **Operator alias + labels** (`PUT /nodes/{id}/meta`, `harnessctl rename|label`, dashboard Rename): bolt `node-meta`, operator-owned (the agent's manifest never overwrites it), validated (alias ≤ 64 printable chars; label keys `^[a-z0-9][a-z0-9._-]{0,62}$`, ≤ 32 labels) so labels can become placement selectors, and cleared in the same transaction as a revocation.
- **Audit log** (`audit.go`, `GET /audit`, `harnessctl audit [-noise]`, dashboard "Audit log"): three independently capped bolt logs. `security` (5,000) is writable **only by operator-credentialed actions** — revocations, invitations, sign-ins, metadata changes, update dispatches, workload submissions (command + argument **count**, never argv) — and `audit()` reroutes anything a peer triggers (actor `node`/`anonymous`), so no peer can evict it; `admissions` (5,000) holds first admissions and *how* (`pairing-token` / `enrollment:<prefix>`), separate because a shared-token holder can mint identities at will (it can churn this log — the answer to that is rotating `-pairing-token`, exactly as for revocation); `noise` (2,000) holds rejections and known-identity reconnects. The default view (`GET /audit`, `harnessctl audit`, dashboard) merges security + admissions by time. Rejection entries carry a NodeID **only once verified** (derived from the key *and* the signature checked out); a pre-verification claim goes in `detail.claimedNodeId`, and `env.Source` (peer-written) never reaches the log. Peer strings are clipped to 128 runes. Rejections are limited per verified identity (or peer *host*, never host:port — reconnects use new ephemeral ports) per 10 minutes via a bounded map, plus a global 30/minute ceiling whose suppression summary is flushed from the heartbeat ticker, so `GET /audit` stays read-only. Operator entries record which credential acted (`operator-token` vs `dashboard-session`). It is an operator's record, not tamper-proof against the token holder.

### 9.1 The relay path in detail

**Problem it solves:** home routers don't accept unsolicited inbound connections, and port-forwarding is exactly the kind of manual, fragile, per-router chore this project has been actively removing. The user explicitly chose a **self-hosted relay** over a third-party mesh VPN (e.g. Tailscale) specifically to avoid a third-party dependency, even though a mesh VPN would have been less code to build.

**How it works:** `cmd/relay` is a small, disposable, standalone binary meant to run somewhere with a real public address (a cheap VPS is simplest; a home box works if its router can port-forward one port to it). Both the manager and a remote agent **dial out** to it — nothing needs an open inbound port at home. `internal/relay` implements a tiny rendezvous protocol: the first line of any connection to the relay is a one-line JSON handshake (`{"role":"listen"|"connect","session":"<token>"}`); the relay pairs a `listen` and a `connect` sharing the same session token and splices their two TCP connections into one raw bidirectional byte stream via `io.Copy` in both directions. **The relay never terminates TLS or parses WebSocket framing** — it only ever sees ciphertext once TLS is in play, which is the whole point: the manager's existing fingerprint pinning and per-node signature (§6) remain the real, unmodified end-to-end security boundary regardless of what the relay does. Authorization at the relay is a single unguessable bearer token per session (treat it with the same care as the pairing token).

`internal/transport/relay` implements `domain.Transport` over that protocol. Its `Listen` (manager side) keeps a **pool of `pendingPoolSize` (3) concurrent "listen" registrations** parked at the relay at all times, replenishing immediately whenever one is consumed — this closes the race where an agent reconnecting right after a drop could otherwise arrive in the gap between a pairing being consumed and the manager noticing it needs to re-register. It reuses `internal/transport/ws`'s WebSocket/TLS *serving* logic via a new `ws.Transport.ListenOn(ctx, net.Listener)` method (extracted from `Listen`, zero behavior change to the pre-existing direct-LAN path — this was a deliberate, reviewed choice over duplicating that logic, since duplication of that specific machinery was judged too drift-prone). The relay's own idle-timeout-driven re-registration is deliberately silent (a distinguished sentinel error, `relay.ErrTimedOut`) rather than logged — on a healthy, idle manager this fires constantly under completely normal operation, and logging it would look like the system is malfunctioning when it isn't.

`internal/transport/multi` lets the manager listen on **both** the direct LAN transport and the relay transport simultaneously — a small fan-in composite `domain.Transport`, composed at `cmd/manager`'s composition root, so neither `internal/transport/ws`, `internal/transport/relay`, nor `manager.Server` needs to know the other listening path exists.

New flags: `-relay-addr`/`-relay-token` on both `cmd/manager` and `cmd/agent` (disabled unless `-relay-addr` is set; `-relay-token` is then required, validated before any disk I/O, matching `-pairing-token`'s existing fail-fast pattern).

**Verified with real binaries, not just tests:** a real `agent.exe`, given only a relay address and token (no LAN address or discovery at all), registered through a real `relay.exe` to a real `manager.exe` over actual TLS, and a `PING` command dispatched through the manager's real HTTP API round-tripped successfully through the whole relay pipe.

**Self-update over relay is now supported.** The relay-backed manager listener serves `/agent-binary`, and relay agents receive an injected HTTP client that opens a separate rendezvous connection for the download. TLS fingerprint pinning remains end to end; the relay still sees only ciphertext in secure mode. Initial remote onboarding still requires placing the first agent binary on the device because external tools such as `curl` do not speak the relay rendezvous handshake.

Relay onboarding scripts are available from both the dashboard and `harnessctl join remote`. Because ordinary bootstrap tools do not speak the rendezvous handshake, the first agent binary must still be placed on an off-LAN device manually; later self-updates are automatic through the relay.

---

## 10. Roadmap — agreed build order (2026-10-01)

Done since the previous version of this list: phone-first one-time enrollment, the relay-carried first download for invitation-enrolled remote devices (`harnessctl join remote` remains a manual-binary path), node revocation, and the operator-API browser guard.

The user asked for *all* remaining work, sequenced for the best order. Each item ends in its own plan → implement → test → review → commit cycle (§12); a later plan may still reorder if something new is learned, but say so explicitly.

**Phase 1 — Secure, onboard-everything foundation**
1. ✅ **Multi-platform agent catalog** — done (see §7, "Agent catalog"). Was: the manager serves one agent binary at a time, so a phone manager can onboard only one platform without a restart, and self-update can't reach a mixed fleet. Serve a catalog (windows/amd64, linux/arm64 for Android, linux/amd64, darwin/arm64, ...) and pick per invitation, join script, and self-update. Goes first because every later increment that changes agent behavior must be rolled out to *every* platform's agents through self-update.
2. ✅ **Operator authentication** — done (see §11, operator API bullet). Was: `guardOperatorAPI` stops browsers, but on Android loopback is shared across apps: any installed app with network permission can call `127.0.0.1:7421` (`POST /workloads` runs code on the fleet; `GET /join-info` reveals the permanent tokens). Non-browser clients pass the guard by design. Sketch: a random operator token in the manager state dir; `harnessctl` reads it from the file; the dashboard gets it via a one-time login URL (printed by the launcher) exchanged for an HttpOnly, SameSite=Strict cookie. Must land before anything makes the API more powerful.
3. ✅ **Fleet hygiene** — done (see §9, "Fleet hygiene"). Was: operator rename/labels, duplicate-physical-host detection (two identities on one machine double-count capacity), and an admission/revocation audit log. Before reservation, so capacity numbers mean what they say.

**Phase 2 — Real distributed compute**
4. ✅ **Capacity reservation, multi-slot nodes, and a work queue** — done (see §7, "Capacity reservation and the queue"). Was: placement reserves cores/memory so concurrent submissions can't oversubscribe; nodes run N concurrent workloads (today's executor allows one); submissions with no free capacity wait instead of being rejected. Must handle a mixed-version fleet: agents advertise a slot count in the manifest, and absent means 1 (today's behavior).
5. ✅ **Artifacts** — done (see §7, "Workload files", and §8). Was: content-addressed, hash-verified, size-capped files in and out of workloads, so tasks are no longer limited to argv in and 64 KiB stdout out. Before jobs, because the headline batch cases (images, documents) need files.
6. ✅ **Fan-out/fan-in batch jobs** — done (see §7, "Batch jobs"). Was: one request → N tasks spread across devices → per-task retry → aggregated result. Builds on 4 and 5.

**Phase 3 — Safe, typed capabilities**
7. **Typed capability catalog + policy.** Allow-listed task types with parameter schemas, dashboard forms instead of raw commands, per-capability policy; raw `system.execute` becomes an explicit advanced opt-in.
8. **LLM vertical slice.** `llm.inventory` / `llm.generate` against a local runtime (Ollama or llama.cpp), model/VRAM-aware placement, streamed results to the phone.

**Phase 4 — Product packaging**
9. **Native Android app (+ agent Windows service).** A foreground service running the manager (no Termux, survives OEM battery policy better) with the existing dashboard in a WebView, QR scanning, and boot start; agents install as a supervised Windows service instead of an HKCU Run entry. Here for effort vs. value: the manager serves the dashboard, so the shell doesn't churn with API changes. Move it up if Termux battery kills start hurting. Feasibility notes: this machine has the Android SDK (platforms 33/36, NDK 28), Flutter, and JDK 17; Android 10+ refuses to exec binaries from app-writable directories, so ship the manager as a `lib*.so` in `jniLibs` (or bind via gomobile). Decide that in the plan.

**Phase 5 — Intelligence**
10. **v6 AI intent planner.** Natural language → a typed job plan (7 + 6) → deterministic policy check → user approval on the phone → execution. Last, because it composes everything above and its safety rests on the typed catalog and policy (baseline principle #5: AI proposes, the harness enforces).

## 11. Known limitations and gotchas — read before touching related code

- **`cmd/agent/main.go` shows as modified in `git status` on the Windows dev machine even with a zero-content diff.** A CRLF-normalization artifact that recurs whenever `git checkout`/certain tools touch that file on that machine. Confirmed harmless via empty `git diff` output. Check `git diff --stat` on this specific file before staging it as part of an unrelated commit.
- **`nhooyr.io/websocket`'s `Conn.Close()` blocks for ~5 seconds** if the peer isn't actively running a receive loop (it waits for a close-handshake ack that never arrives). This is why several transport-layer tests take ~5s each — expected, not a hang. If a test needs something to happen *immediately* after closing a connection, fire the close in a goroutine rather than blocking on it (which is also a more faithful simulation of a real dropped connection anyway).
- **The manager's TLS certificate must be ECDSA P-256, never Ed25519.** Windows schannel (curl.exe, PowerShell `Invoke-WebRequest`, anything that isn't Go's own `crypto/tls`) cannot complete a handshake against an Ed25519 certificate at all — confirmed via hardware testing, no client-side workaround exists. `internal/mtls` already does this correctly; don't "simplify" it back to Ed25519.
- **Bare command names crash the entire agent process on Android/Termux**, not just the one workload — Go's `exec.Command` → internal `LookPath` → `syscall.Eaccess` → blocked by Android's seccomp policy → SIGSYS kills the whole process. Fixed via `internal/agent/executor.go`'s `resolveCommandPath` (manually searches `$PATH` via `os.Stat`, sidestepping `Eaccess`, non-Windows only). If a similarly mysterious whole-process death shows up on another constrained platform, check for this exact pattern first.
- **The first download over the relay needs the public enrollment gateway.** Invitation-enrolled remote devices fetch the agent through the relay's browser-trusted HTTPS gateway (`cmd/relay -public-addr`); the older `harnessctl join remote` path still requires placing the binary by hand, since ordinary `curl` cannot perform the rendezvous handshake. Self-update after install works over the relay either way.
- **One live agent process per identity** (`internal/instancelock`, taken in `cmd/agent`): every generated launcher restarts the agent on exit, and self-update used to relaunch the new binary itself too — two live copies of one identity. Launchers now set `HOME_HARNESS_SUPERVISED=1`, so after a swap the agent just exits and the launcher starts the new binary; and an OS file lock (flock / LockFileEx, kernel-released even on a crash) in the identity dir makes any second copy (an old launcher without the marker, a manual run) wait as a standby. Keep the lock in `cmd/agent`, not `Agent.Run`: in-process tests reuse identity dirs across sequential agents.
- **launchd/cron give a minimal PATH**, so the Unix launcher freezes the installing shell's PATH into `run-agent.sh`; otherwise Homebrew/`/usr/local/bin`/ollama commands are "not found" only when run as a workload.
- **Android agents must be `GOOS=linux` builds.** A `GOOS=android` build reports `android/arm64`, matches no catalog entry, and silently never gets updates (`scripts/build-agents.sh` does the right thing).
- **Agents predating the catalog only ever fetch `/agent-binary`.** That route serves the primary build (windows/amd64 when loaded, regardless of flag order), which is exactly what every pre-catalog agent in the field is; their one update onto the new protocol was verified with a real pre-catalog `agent.exe`.
- **Relay connects wait briefly for a listener.** The manager's pool slots expire together at the relay's idle timeout and re-dial in lockstep, so the relay holds an unmatched `connect` for `connectGrace` (500ms) instead of failing instantly; a connect for a session with no manager still fails fast.
- **Downloaded self-update binaries must be created with `0o755`**, not `os.Create`'s default `0o644` — invisible on Windows (no POSIX exec bit concept) but breaks every Linux/Termux self-update relaunch with "permission denied." Already fixed in `internal/agent/selfupdate.go`'s `downloadFile`.
- **This dev machine has no cgo/gcc**, so `go test -race` fails with "requires cgo." Use plain `go test -count=2` (or higher) for flake-hunting instead of relying on the race detector here.
- **The operator API (`127.0.0.1:7421` default) is loopback-only *and* authenticated** — three independent layers, all on `NewHTTPHandler`'s mux, so new operator routes must be registered there and must never change state on GET:
  1. **Host allow-list + `http.CrossOriginProtection`** (`apiguard.go`): stops a browser on the same machine being used as a proxy (CSRF; DNS rebinding). Before this, any visited page could run commands on the fleet — reproduced in headless Chrome.
  2. **Operator token** (`operatorauth.go`): every route except `GET /` (the static dashboard shell), `POST /login`, and the token-scoped `GET /enrollments/{token}/qr` needs `Authorization: Bearer` with either the raw token (from `-operator-token-file`, default `harness-operator-token`, 0600, created on first run) or the dashboard session `HMAC(token, "harness-dashboard-session-v1")`. This closes the hole the guard deliberately leaves open: on Android, loopback is shared by *every installed app*, which before this could submit workloads, revoke nodes, or read `/join-info`'s tokens (mutation-tested: with auth off, an uncredentialed client did all three).
  3. **No cookies, ever.** Cookies aren't isolated by port (RFC 6265 §8.5; SameSite ignores port), so a cookie for `:7421` would also be sent to an app listening on any other `127.0.0.1` port — verified in real Chrome with a canary cookie. The dashboard keeps its session in `localStorage` (origin-scoped, port included) and sends it only as a header from its own `api()` wrapper; `/events` is read with a fetch-streaming SSE reader because `EventSource` can't send headers. Consequences to preserve: the dashboard must never load external scripts, and must render every agent-supplied value escaped (`escapeHtml`/`textContent`; class names via the whitelisting `stateClass`) — enrolled agents control node names, workload output, and reported states, and script injection there would steal the operator's credential from `localStorage` (the manager also rejects workload states an agent can't legitimately report); "Sign out" only clears that browser's copy; revoking every client means deleting the token file and restarting.

  Sign-in: the manager logs `http://127.0.0.1:7421/#login=<token>` at startup (the fragment never reaches a server; the dashboard trades it for a session and scrubs it from the URL/history). The Termux installer opens it with `termux-open-url`. `harnessctl` reads the same token file from its working directory (`-token-file`) or `HARNESS_OPERATOR_TOKEN`; `harnessctl login-url` prints the link. Widening `-api-addr` beyond loopback is still unsupported: the browser guard's Host check and the plain-HTTP transport both assume a local listener.
- **Placement always breaks ties by NodeID, not randomly** — deterministic. Back-to-back submissions no longer pile onto one node: placement counts what is already reserved on each node (§7, "Capacity reservation and the queue"), so this is only a tie-break now. Live memory still lags by a heartbeat, which is why the memory check adds reservations on top of it (conservative).
- **Workload files land without an exec bit, and on Windows a bare command name is not looked up in the working directory.** So `./run.sh` or `job.bat` as the command won't work; run scripts through their interpreter: `sh job.sh`, `powershell -NoProfile -File job.ps1`, `python job.py`.
- **A node's slot count is the agent's choice** (`-slots`, default half the logical CPUs). Lower it on a laptop you're using, raise it for many light tasks; the manager learns it at the next (re)connect.

---

## 12. Working conventions established in this project (worth preserving)

- **Plan before implementing anything non-trivial** — explore the relevant code first, write a concrete plan, get it approved, then implement. Don't assume scope; ask when a design choice has real operational tradeoffs (e.g., "self-hosted relay vs. third-party mesh VPN" was an explicit question put to the user, not a unilateral technical call).
- **Full test suite run at least twice after any change** (`go test ./... -count=2`), zero flakes is the bar.
- **A second, critical review pass before declaring anything done** — this project has repeatedly caught real bugs this way (a TLS cert algorithm incompatibility, a race in address validation, a shell script bug, log-noise issues, a missing end-to-end verification step). Don't skip this step to save time; it has never once come back clean-with-nothing-to-fix.
- **For anything touching real OS/network/hardware behavior, build and run the actual binaries end-to-end**, not just package-level unit tests. This project has been burned multiple times by bugs that only exist on real hardware/OS combinations that pure unit tests (especially on a single Windows dev machine) cannot surface: the Ed25519/schannel incompatibility, the Android `LookPath`/seccomp crash, the missing executable bit on downloaded binaries. "The tests pass" is not the same claim as "this works," and this codebase's history is the reason why.
- **Document deferred scope explicitly, in code comments and/or commit messages, rather than dropping it silently.** Every increment in this project has an explicit "what we did NOT do and why" list (see §9.1's relay limitations as the most recent example) rather than leaving gaps for someone to discover later by surprise.
- **Commit messages should explain *why*, not just *what*** — several past bugs and the specific real-world friction that motivated each increment are recorded there; reading recent `git log` output for this repo is a legitimate and useful way to recover context this document doesn't capture.

---

## 13. How to build and run everything locally

```powershell
# from the repo root
go build -o bin\manager.exe .\cmd\manager
go build -o bin\agent.exe .\cmd\agent
bash scripts/build-agents.sh   # every platform's agent into bin/agents/ (CGO_ENABLED=0; Android = GOOS=linux)
go build -o bin\relay.exe .\cmd\relay
go build -o bin\harnessctl.exe .\cmd\harnessctl

# LAN-only manager (no relay), able to onboard and update Windows and Android agents
.\bin\manager.exe -pairing-token <secret> -agent-binary bin\agents\agent-windows-amd64.exe -agent-binary bin\agents\agent-linux-arm64
# prints its TLS fingerprint on startup — copy it for the agent below

# LAN agent, same machine/network
.\bin\agent.exe -pairing-token <secret> -manager-fingerprint <fingerprint-from-above>

# manager with relay enabled, in addition to LAN
.\bin\relay.exe -addr <public-or-loopback-addr-for-testing>
.\bin\manager.exe -pairing-token <secret> -relay-addr <relay-addr> -relay-token <relay-secret>

# remote agent, reachable only via the relay (no LAN address needed at all)
.\bin\agent.exe -pairing-token <secret> -manager-fingerprint <fingerprint> `
    -relay-addr <relay-addr> -relay-token <relay-secret>

# operator CLI (defaults to the manager's loopback API at 127.0.0.1:7421). It needs the
# manager's operator token: run it from the manager's working directory (where
# harness-operator-token was created), or pass -token-file / set HARNESS_OPERATOR_TOKEN
.\bin\harnessctl.exe nodes
.\bin\harnessctl.exe run <node-id> <command...>
.\bin\harnessctl.exe join <manager-lan-addr> [android]   # generates a onboarding script

# local web dashboard (loopback-only): open the sign-in link the manager logs at startup
# (http://127.0.0.1:7421/#login=...), or print it with:
.\bin\harnessctl.exe login-url
```

Full test suite: `go test ./... -count=2` from the repo root (add `-v` for verbose output; `-race` is **not** available on this dev machine, see §11).

---

## 14. Suggested first actions for whoever picks this up

1. Read this document fully, then skim `v1.md` §22 and `Home_Compute_Harness_Baseline_Architecture.md` for the original vision framing.
2. Run `git log --oneline -30` to see the recent commit sequence and confirm this document still matches reality (it was written immediately after the most recent commit, but code moves faster than docs).
3. Run the full test suite once to confirm a clean baseline before making any change.
4. Before starting new work, confirm with the user which of §10's options (or something else entirely) is actually wanted next — do not assume Option A just because it's listed first here.
