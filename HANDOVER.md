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
- **`WorkloadState`**: `PENDING | RUNNING | COMPLETED | FAILED | CANCELED | UNKNOWN` (`UNKNOWN` = status couldn't be resolved after a manager restart — an orphaned in-flight workload).
- **`WorkloadStatus`**: `Stdout/Stderr` (capped at 64KB, `Truncated` flag), `ExitCode`, `Error`, `StartedAt`, `FinishedAt`.
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
`nodeFits(rec, capability, requirements)`: capability match is checked unconditionally; if requirements need *live* metrics (memory/CPU%) and the node has never heartbeated, it's ineligible. `selectNode` filters via `nodeFits` then picks the candidate with the **most available memory** — the single live signal used today — breaking ties by ascending NodeID for determinism. This is deliberately simple, not a multi-factor scorer. Known limitation worth knowing before extending it: determinism can make back-to-back submissions repeatedly target the same node faster than heartbeats can update its metrics, which combined with "one workload per node" today means the second submission can be rejected — a real, understood rough edge, not a bug to "fix" without discussing the tradeoff with the user first.

### Workload lifecycle + reconciliation (`workloads.go`, `reconcile.go`) — v3's services model
`WorkloadRegistry` tracks `WorkloadRecord{Workload, Status, Restart}` keyed by `WorkloadID`. `FailInFlightFor(nodeID)` fails any PENDING/RUNNING workload when its node drops. `RestartCandidates(now)` filters by `RestartPolicy.WantsRestartAfter` plus an elapsed `NextRestartAt`. `MarkRestarting` resets status to PENDING, clears prior output, bumps the lifetime `Count`, and resets or increments `BackoffCount` depending on whether the previous run was "healthy" (lasted ≥ 5 minutes, `restartHealthyRunThreshold`). `DeferRestart` bumps `NextRestartAt` *without* counting it as a real restart attempt — used for benign placement failures (e.g. no eligible node right now) so those don't chew through the backoff budget.

Reconciliation runs on its own ticker (`ReconcileInterval`, default 5s — separate from heartbeat monitoring). Each tick: find restart candidates → resolve target (pinned workloads stay on their node; unpinned ones re-run placement) → on success, `MarkRestarting` + re-dispatch `WORKLOAD_ASSIGN`; either way, `DeferRestart` schedules the next check using exponential backoff (`backoffFor`: base 5s, doubling per attempt, capped at 5 minutes, with an overflow guard).

### HTTP API and dashboard (`api.go`, `dashboard.go`, `join.go`, `binaryplatform.go`, `selfupdate.go`)
The manager runs **two separate HTTP surfaces**:
1. The agent-facing transport (`:7420` by default) — where `domain.Transport.Listen` actually accepts node connections. Self-update's binary download (`/agent-binary`) is registered on *this* listener via `ws.Transport.Handle`, deliberately reusing the exact port agents already dial rather than opening a new one.
2. The operator-facing HTTP API (`127.0.0.1:7421` by default, **loopback-only** — this is a hard trust-model line, see §11) — `GET /nodes`, `GET /nodes/{id}`, `GET /events` (SSE), `POST /nodes/{id}/commands`, `POST /nodes/{id}/update`, `GET /agent-binary/hash`, `GET /join-info`, `GET /join-script`, `GET /` (the dashboard), `POST /workloads` and friends.

`GET /` serves a single self-contained embedded HTML/CSS/JS file (`dashboard.go`'s `//go:embed dashboard.html`) — no build step, no CDN dependency (this project is local-first by design, so the dashboard must work with zero external network reachability). It lists nodes (live via the SSE stream, no polling loop), shows a platform-aware "(outdated)" marker (see `binaryplatform.go` — detects the served binary's OS/arch from its PE/ELF header, so a Linux node's hash is never wrongly compared against a Windows binary's hash), and has an "Add a device" form that calls `GET /join-script` to generate a copy-pasteable onboarding script.

`internal/joinscript` is the **single source of truth** for onboarding-script text (PowerShell for Windows, bash for Android/Termux), used identically by `harnessctl join` and the dashboard's endpoint — this avoids the two ever silently drifting apart.

---

## 8. Agent internals (`internal/agent`)

### Connection lifecycle (`agent.go`)
`Agent.Run(ctx)` loops `connectAndServe` with **exponential backoff**: starts at `Config.ReconnectBackoff`, doubles on each failure up to `MaxReconnectBackoff`, and resets to the starting value once a connection has stayed up longer than `3 × HeartbeatInterval` (a "real session," not a flash-in-the-pan reconnect). `resolveManagerAddr`: if `Config.ManagerAddr` is set, use it directly and never consult `Discoverer`; otherwise ask `Discoverer.Discover(ctx)` on every (re)connect attempt (not just once — the manager's address may change across restarts).

### Command and workload execution (`commands.go`, `workloads.go`, `executor.go`)
Two distinct paths:
- **Commands** are stateless request/reply (`handleCommand`/`executeCommand` in `commands.go`) — `PING`, `ECHO`, `GET_SYSTEM_INFO`, etc. `SELF_UPDATE` is special-cased to **ack synchronously before** `performSelfUpdate` runs (so the ack isn't lost when the process replaces itself).
- **Workloads** are longer-running (`handleWorkloadAssign` in `workloads.go` → `Executor.Start`, which dispatches to `startExecute` (`system.execute`) or `startFilesystemRead` per the workload's effective capability). The executor streams status back (`RUNNING` as soon as the subprocess starts, then a terminal state) rather than blocking until completion.
- `InsecureWorkloadsDisabled`: an agent run with its own `-insecure` refuses to execute workloads at all, even if a manager (which it can't verify) tries to assign one — this is a deliberate refusal to run arbitrary code for an unauthenticated peer.

### Self-update (`selfupdate.go`)
Downloads a new binary over a direct or relay-mediated HTTP(S) connection, verifies its SHA-256 against the expected hash, sets it executable (`0o755` — **do not regress this to `os.Create`'s default mode**, see §11), and relaunches with the exact flags the process was originally started with (`os.Args[1:]`, captured as `LaunchArgs`). Relay downloads use an injected HTTP client so the agent core does not depend on the concrete relay transport.

---

## 9. Onboarding and connectivity paths — current state

There are now three distinct ways a node joins a manager, in the order they were built:

1. **LAN, manual `-manager-addr`/`-manager-fingerprint` flags** (v0/v1 era) — always available, most explicit.
2. **LAN, UDP multicast discovery** (`internal/discovery/udp`) — zero-config *if* the device is on the same broadcast domain; genuinely LAN-scoped (multicast TTL=1), cannot help a remote device.
3. **LAN, one-time `harnessctl join`/dashboard-generated script** (v5 part 1/1b/2) — operator runs a generated PowerShell (Windows) or bash (Android/Termux) script once; handles TLS fingerprint + pairing token and installs an automatic launcher. Windows uses the current user's `HKCU\...\Run` entry plus a hidden retry loop; Android uses Termux:Boot and a wake lock. LAN launchers deliberately omit `-manager-addr`, so the existing multicast discoverer follows a phone whose Wi-Fi address changes. `internal/joinscript` is the shared script-generation logic; the manager's local **web dashboard** (`GET /`, loopback-only) is the friendliest way to generate one, reusing `GET /nodes` + `GET /events` (SSE) + `GET /join-script`.
4. **Off-LAN, relay-based** (v5 remote part 1, most recent) — for a device that isn't on the manager's LAN at all (different network, mobile data). See §9.1.
5. **Phone-first QR/link enrollment** — the loopback dashboard creates a 10-minute, single-use invitation for Windows or Android. LAN invitations point at the phone directly; internet invitations use the relay's browser-trusted HTTPS gateway. The QR/link contains no permanent pairing token or manager relay session. A relay enrollment receives an opaque, authenticated per-device relay credential that remains usable across relay restarts when `-alias-key` is preserved.

### Admission vs. reconnect, and revocation

Admission and reconnection are different credentials. A previously unknown identity needs an admission credential at `REGISTER` (the shared `-pairing-token`, or a consumed one-time invitation token); a known identity reconnects on proof-of-possession of its Ed25519 key alone (`handleRegister`). Because of that, rotating the pairing token no longer evicts anyone, so **revocation** (`internal/manager/revocation.go`) is the way to take an admission back: `POST /nodes/{id}/revoke`, `harnessctl revoke <id>`, or the dashboard's per-node *Revoke* button. A revocation is a persisted denylist entry (bolt `revoked` bucket, written in the same transaction that forgets the node's record) checked in `handleRegister` *before* any token is considered, under `admitMu` so a racing `REGISTER` can't re-add a node mid-revoke. Revoking closes the live connection (the agent cancels its running workload on disconnect), fails in-flight unpinned work (its restart policy may re-place it elsewhere), and cancels workloads pinned to the node (otherwise the reconciler would defer them forever). `DELETE /revocations/{id}` / `harnessctl unrevoke` lifts it; the node then needs a fresh admission (a shared-token launcher does this on its own; an invitation-enrolled node needs a new invitation). Limits, stated wherever revocation is offered: it denies one *identity* (a shared-token holder can mint a new one, so also rotate `-pairing-token`), and an `ha1.` relay alias stays usable at the relay until `-alias-key` is rotated (the manager still rejects it at `REGISTER`). A revoked agent keeps retrying at its normal capped backoff (30s), each attempt logged as `registration rejected: node revoked by operator`.

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
1. **Multi-platform agent catalog.** The manager serves one agent binary at a time, so a phone manager can onboard only one platform without a restart, and self-update can't reach a mixed fleet. Serve a catalog (windows/amd64, linux/arm64 for Android, linux/amd64, darwin/arm64, ...) and pick per invitation, join script, and self-update. Goes first because every later increment that changes agent behavior must be rolled out to *every* platform's agents through self-update.
2. **Operator authentication.** `guardOperatorAPI` stops browsers, but on Android loopback is shared across apps: any installed app with network permission can call `127.0.0.1:7421` (`POST /workloads` runs code on the fleet; `GET /join-info` reveals the permanent tokens). Non-browser clients pass the guard by design. Sketch: a random operator token in the manager state dir; `harnessctl` reads it from the file; the dashboard gets it via a one-time login URL (printed by the launcher) exchanged for an HttpOnly, SameSite=Strict cookie. Must land before anything makes the API more powerful.
3. **Fleet hygiene.** Operator rename/labels, duplicate-physical-host detection (two identities on one machine double-count capacity), and an admission/revocation audit log. Before reservation, so capacity numbers mean what they say.

**Phase 2 — Real distributed compute**
4. **Capacity reservation, multi-slot nodes, and a work queue.** Placement reserves cores/memory so concurrent submissions can't oversubscribe; nodes run N concurrent workloads (today's executor allows one); submissions with no free capacity wait instead of being rejected. Must handle a mixed-version fleet: agents advertise a slot count in the manifest, and absent means 1 (today's behavior).
5. **Artifacts.** Content-addressed, hash-verified, size-capped files in and out of workloads, so tasks are no longer limited to argv in and 64 KiB stdout out. Before jobs, because the headline batch cases (images, documents) need files.
6. **Fan-out/fan-in batch jobs.** One request → N tasks spread across devices → per-task retry → aggregated result. Builds on 4 and 5.

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
- **Relay connects wait briefly for a listener.** The manager's pool slots expire together at the relay's idle timeout and re-dial in lockstep, so the relay holds an unmatched `connect` for `connectGrace` (500ms) instead of failing instantly; a connect for a session with no manager still fails fast.
- **Downloaded self-update binaries must be created with `0o755`**, not `os.Create`'s default `0o644` — invisible on Windows (no POSIX exec bit concept) but breaks every Linux/Termux self-update relaunch with "permission denied." Already fixed in `internal/agent/selfupdate.go`'s `downloadFile`.
- **This dev machine has no cgo/gcc**, so `go test -race` fails with "requires cgo." Use plain `go test -count=2` (or higher) for flake-hunting instead of relying on the race detector here.
- **The operator-facing HTTP API (`127.0.0.1:7421` default) is intentionally loopback-only.** `POST /nodes/{id}/commands` and `POST /workloads` have no authentication of their own — the trust model is "same-machine access is already fully privileged." Widening `-api-addr` to the LAN would expose unauthenticated arbitrary code execution on every registered node. If LAN-wide dashboard access is ever wanted, it needs a real auth layer first — this has been explicitly scoped out of every increment so far, not an oversight. **Loopback does not keep out a browser on the same machine**, and the manager's phone also browses the web: until `internal/manager/apiguard.go`, any visited page could fire a no-preflight `text/plain` POST at `/workloads` (verified with real headless Chrome: a cross-origin page ran a command on a registered node) or, via DNS rebinding, read `/join-info`'s tokens. Every operator route is now wrapped in `guardOperatorAPI` — Go's `http.CrossOriginProtection` for non-safe methods plus a Host allow-list (IP literals and `localhost` only). Keep new operator routes on `NewHTTPHandler`'s mux so they inherit it, and never perform state changes on GET.
- **Placement (`selectNode`) always breaks ties by NodeID, not randomly** — deterministic, but can repeatedly target the same node for back-to-back submissions faster than heartbeats refresh its metrics (see §7's placement section). Understood, not yet addressed.

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
go build -o bin\relay.exe .\cmd\relay
go build -o bin\harnessctl.exe .\cmd\harnessctl

# LAN-only manager (no relay)
.\bin\manager.exe -pairing-token <secret>
# prints its TLS fingerprint on startup — copy it for the agent below

# LAN agent, same machine/network
.\bin\agent.exe -pairing-token <secret> -manager-fingerprint <fingerprint-from-above>

# manager with relay enabled, in addition to LAN
.\bin\relay.exe -addr <public-or-loopback-addr-for-testing>
.\bin\manager.exe -pairing-token <secret> -relay-addr <relay-addr> -relay-token <relay-secret>

# remote agent, reachable only via the relay (no LAN address needed at all)
.\bin\agent.exe -pairing-token <secret> -manager-fingerprint <fingerprint> `
    -relay-addr <relay-addr> -relay-token <relay-secret>

# operator CLI (defaults to the manager's loopback API at 127.0.0.1:7421)
.\bin\harnessctl.exe nodes
.\bin\harnessctl.exe run <node-id> <command...>
.\bin\harnessctl.exe join <manager-lan-addr> [android]   # generates a onboarding script

# local web dashboard (loopback-only)
# open http://127.0.0.1:7421/ in a browser
```

Full test suite: `go test ./... -count=2` from the repo root (add `-v` for verbose output; `-race` is **not** available on this dev machine, see §11).

---

## 14. Suggested first actions for whoever picks this up

1. Read this document fully, then skim `v1.md` §22 and `Home_Compute_Harness_Baseline_Architecture.md` for the original vision framing.
2. Run `git log --oneline -30` to see the recent commit sequence and confirm this document still matches reality (it was written immediately after the most recent commit, but code moves faster than docs).
3. Run the full test suite once to confirm a clean baseline before making any change.
4. Before starting new work, confirm with the user which of §10's options (or something else entirely) is actually wanted next — do not assume Option A just because it's listed first here.
