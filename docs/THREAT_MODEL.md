# Threat model

Home Harness runs code on other people's machines at the request of one manager. This page says what is protected, from whom, by what, and what is left. File names point at the code.

## 1. What the system trusts

The **manager** trusts its operator and nothing else. It never trusts an agent, a relay, a model's answer, or a web page. Agents trust the manager only after pinning its TLS certificate fingerprint. A relay is a dumb pipe: it cannot read or change what passes through (TLS to the manager is end to end).

## 2. Assets

| Asset | Where it lives | What loss means |
|---|---|---|
| Device keys (Ed25519) | agent identity dir, 0600 (`internal/identity`) | someone can pose as that device |
| Operator token | `-operator-token-file`, 0600 (`operatorauth.go`) | full control of the fleet: code on every device |
| User tokens | shown once; only SHA-256 stored in the database (`users.go`) | that user's role |
| AI key | `ai-key` file (`openai.go`) | free use of the fleet's models, nothing else |
| Remote key | `remote-access` file (`remote.go`) | remote dashboard at its mode's power |
| Pairing token | flag / `pairing-token` file / database (`secrets.go`) | a stranger can add a device |
| Manager TLS key | `harness-manager-tls/`, 0600 (`internal/mtls`) | pose as the manager to agents |
| Artifacts | artifact dir, 0700 (`internal/artifacts`) | workload inputs and results |
| Audit log | database (`audit.go`) | loss of the record of who did what |
| Model prompts and answers | memory, plans, workload params | privacy of what users ask |
| Database, state zip | `-db`, `-export-state` output | everything above except user tokens |

## 3. Trust boundaries

1. Operator client to operator API (loopback, bearer token, role per route).
2. Agent to manager (pinned TLS, then a signed REGISTER).
3. Device to join page (plain HTTP by design, see 6).
4. Manager to relay to remote browser (sealed requests; the relay sees only ciphertext).
5. Manager to a model (answers are untrusted text).
6. Agent to a task's process or container (the sandbox).

## 4. Attackers

- **Another device on the LAN.** Can reach the agent port, the join page and the discovery beacon, and can sniff the join page.
- **A malicious enrolled agent.** Holds a valid device key and runs workloads.
- **A malicious relay.** Sees traffic metadata, can drop, delay, replay.
- **A compromised browser tab or another app on the manager machine.** Can aim requests at 127.0.0.1 (on a phone every app shares it).
- **A person with file access on the manager PC.** Can read state files.
- **A malicious plan from an AI model.** The planner reads attacker-influenced text and proposes jobs.

## 5. STRIDE

| Threat | Mitigation (and where) | Residual |
|---|---|---|
| **S** a device poses as another | NodeID is derived from the public key; REGISTER carries a signature over the pairing token and ID (REGISTER handling in `server.go`); revocation list (`revocation.go`) | a device with a stolen key file |
| **S** a fake manager | agents pin the certificate fingerprint; first-use pin is stored (`cmd/agent/firstuse.go`); pairing codes derived from the fingerprint expose a man in the middle at approval (`protocol.PairingCode`) | first contact on a hostile network when the operator skips the code check |
| **S** an app calls the operator API | bearer token on every route but a short public list (`operatorPublic`); no cookies; Host and cross-site checks (`apiguard.go`) | an app that can read the token file |
| **S** a login guess | 256-bit tokens, constant-time compare, failed attempts get 429 per source (`authLimiter`); a valid token is never throttled, so a hostile local app cannot lock the owner out | online guessing is hopeless by size, not blocked by lockout |
| **T** a request changed in transit | TLS to the manager; remote calls are AEAD-sealed per session with replay window (`internal/remoteaccess`) | plain-HTTP join page (6) |
| **T** a forged dashboard session | sessions are HMACs keyed from the operator token file, not from the database; they carry an expiry and the user's token epoch (`users.go`) | a copy of both the token file and the database |
| **T** a result forged by an agent | spot checks re-run a sample on other devices and mark disagreeing devices suspect (`spotcheck.go`) | colluding or deterministic-wrong devices |
| **R** an action denied | every operator action is audited with the user's name as actor (`audit.go`, `operatorauth.go`); the security log is capped but kept apart from peer noise | anyone with the operator token can delete the database; the log is a record, not a defence against the owner |
| **I** a secret leaks through the API | role table: viewers never see tokens, pairing codes, the AI key or join info (`roles.go`); the dashboard escapes all agent text; `/join-info` and friends are admin-only | the admin sees all |
| **I** a user token leaks from a database copy | only SHA-256 hashes are stored | the operator token file is still a secret |
| **I** logs | tokens are never logged; audit stores token prefixes of invitations only | prefixes (8 chars) |
| **D** flooding the agent port | message size cap 4 MiB; rejection log rate limits; per-key bounded maps | a LAN host can still fill sockets |
| **D** huge API bodies | 8 MiB cap on every API body except artifact upload, which has its own size cap | |
| **D** a runaway workload | policy max runtime, output cap 64 KiB per stream (`domain.OutputCapBytes`), container CPU/memory/pids limits (`internal/container/engine.go`) | raw `system.execute` has no memory limit (off by default) |
| **E** a lower role does more | one central role table checked after authentication, unlisted routes need admin; remote modes are held to roles too (`remoteGate`) | other agents' new routes fail closed until listed (tests enforce it) |
| **E** a task escapes | typed tasks are fixed programs with checked parameters; containers drop all capabilities, read-only root, no network unless allowed, non-root | external tools (ffmpeg, ollama) have their own bugs |
| **E** a model's plan runs something bad | the planner may only pick enabled, non-raw catalog types, parameters are re-validated by `Compile`, and a person approves before anything runs (`planner.go`) | an approving person who does not read |

## 6. Known soft spots (by design)

- **The join page and installer scripts are plain HTTP.** This avoids a certificate warning on every phone. The pairing code and the agent hash check protect admission and pinning, not the download. Add devices on a network you trust, with the join window closed otherwise.
- **The legacy dashboard session** (`HMAC(token)`, no expiry) is still accepted so old tabs and the Android app keep working. It dies when the operator token is rotated. New sign-ins get expiring sessions.
- **Windows and permissions.** Go's 0600/0700 are ignored on Windows; files there rely on the user profile's ACLs (private to the user by default). If the state directory is outside the profile, set ACLs yourself.
- **Agent log file** was 0644; now 0600.
- **TLS certificate rotation** needs a protocol change: see `docs/tls-rotation-design.md`.
- **Shared pairing token** lets anyone holding it add a device (while the join window is irrelevant for token joins). Prefer approval joins and invitations; rotate with `harnessctl rotate pairing-token`.

## 7. Hardening checklist for operators

1. Keep `-api-addr` on loopback. Never forward it.
2. Give each person a user (`harnessctl user add NAME viewer|operator|admin`); keep the operator token for emergencies.
3. Set an expiry for guests: `harnessctl user add NAME viewer 7`.
4. Close the join window when not adding devices; add devices on a trusted network only.
5. Prefer `join-requests` approval and compare the pairing code on both screens.
6. Leave raw commands off (`policy`), enable typed task types one by one.
7. Rotate: `harnessctl rotate operator-token` (everyone signs in again), `ai-key` (apps need the new key), `pairing-token` (new devices only). After removing a person: `harnessctl user remove NAME`; if they held the pairing token, rotate it; if they were an admin, rotate the operator token too.
8. Revoke lost devices; rotate the pairing token if the device held it.
9. Keep the state folder private, and delete `-export-state` zips after use.
10. Build releases with Go 1.27.2 or newer (see `docs/THIRD_PARTY.md`).
11. Turn remote access on read-only unless you need more; look at `harnessctl remote log`.
12. Run containers only from images you trust; keep `network` off.

## 8. What each rotation does

| Command | Invalidated | Not affected |
|---|---|---|
| `rotate operator-token` | the old token, every dashboard session (user sessions too: their signing key derives from it) | user tokens, devices, AI key |
| `rotate ai-key` | the old key in apps; a standby gets the new one at its next copy | everything else |
| `rotate pairing-token` | the old token as a way to join | devices already added (they reconnect with their own key), invitations, approvals |
| `user token NAME` | the old token at once; the user's browsers | other users |
| `user rotate NAME [mins]` | browsers now; the old token after the grace time | other users |
| `user remove NAME` | token and sessions | other users |

The pairing token is also saved in the database and in the `pairing-token` file beside it; a rotated value then wins over the `-pairing-token` flag on restart.

## 9. Residual risks, not fixed here

- Brute-force lockout is deliberately absent (see S above).
- The sandbox for raw `system.execute` is none; it is policy-gated and off by default.
- No per-user quota on workloads.
- No TLS certificate rotation without re-pinning every device.
- The audit log can be wiped by the holder of the operator token.
- Windows ACL hardening of the state directory is left to the installer.
