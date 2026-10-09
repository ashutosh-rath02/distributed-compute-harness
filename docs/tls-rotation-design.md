# Rotating the manager's TLS certificate: design (not implemented)

Status: **design only.** Implementing it safely needs an agent change that has to ship to the whole fleet first, and a rollback path that has not been built. Nothing in the code does this today.

## The problem

Agents pin the manager's certificate fingerprint (`internal/mtls` `PinnedClientConfig`; first-use pin in `cmd/agent/firstuse.go`). Replacing the certificate (expiry, key leak, new machine) makes every agent refuse the connection, and an offline agent cannot be told anything. Today the only fix is to re-pin every device by hand.

## What else depends on the fingerprint

- **Pairing codes** are derived from it (`protocol.PairingCode`): pending join requests change code at a switch.
- **Join info and installer scripts** embed it (`/join-info`, `internal/joinscript`), as do enrolment invitations already handed out.
- **Failover**: the cert signs failover terms (`Config.TLSCert`, `standby.go`); a standby pins the primary by fingerprint (`-standby-fingerprint`) and receives the cert and key when promoted. The standby link is separate from the agent link.
- **The relay path**: relayed agents pin the same certificate inside the relay tunnel.
- **Remote dashboard**: the relay gateway stores the manager's fingerprint (`PublishRemoteDashboard`).
- **Android app** and any saved launcher with `-manager-fingerprint`.

## Protocol (two-phase, announce then switch)

Gate: `Manifest.AgentFeatures` contains `tls.rotate.v1`. Agents without it are never rotated onto a new key silently; they appear in the operator's checklist as "needs re-pin".

1. **Prepare.** The operator runs `harnessctl tls rotate`. The manager generates a new ECDSA P-256 certificate (never Ed25519: Windows schannel) and stores it as *next* beside the current one. Nothing is served yet.
2. **Announce.** Over each already-authenticated connection, the manager sends a new message `TLS_ROTATE_ANNOUNCE {nextFingerprint, notBefore, term, seq}` signed with the *current* certificate key (ECDSA over a canonical encoding that includes the agent's NodeID and a fresh nonce, so it cannot be replayed to another agent or reused later). The agent verifies the signature against the public key of the certificate it is connected to (the pinned one), then stores `next` as a second accepted pin and acks with its NodeID signed by its device key.
3. **Collect acks.** The manager tracks which nodes acked. An agent that reconnects before the switch gets the announcement again at REGISTER_ACK. The manager shows "N of M devices have the new pin".
4. **Switch.** When the operator chooses (or all online devices acked and a minimum wait has passed), the manager serves the new certificate for new connections. Agents accept either pin from now on; the old one is dropped by the agent after it has connected successfully once with the new one. Existing connections stay open until they end.
5. **Retire.** After a grace period (default 30 days) the manager stops serving the old cert and the agents forget the old pin.

Dependants are updated at the switch, in this order: standby (pushed `nextFingerprint` through the existing authenticated standby channel before step 4, so it pins the new cert, and it receives the new cert and key with the next copy); relay gateway record (`PublishRemoteDashboard` again); join info and scripts (generated from `Config.Fingerprint` at request time, so they follow automatically); pending pairing codes (shown again, requests re-asked).

## Failure modes

| Failure | Result | Handling |
|---|---|---|
| Agent offline through the whole window | after retirement it cannot connect | keep the old cert serving until the last known device has acked; the checklist lists stragglers; the operator can extend the grace or re-pin by hand |
| Announcement forged by a relay or LAN attacker | rejected: signature must verify under the pinned key | none needed |
| Replay of an old announcement | rejected: nonce and `seq` must exceed the last seen | agent stores highest `seq` |
| Manager key already stolen | the attacker can sign announcements too | rotation cannot help here; the only cure is manual re-pin of every device out of band |
| Switch happens, new cert is broken | online agents fall back to the old pin (still accepted) and keep working | keep the old cert loaded until retirement; `harnessctl tls rollback` reverts the switch |
| Crash between steps | state is persisted (`next`, acks, switched flag) in the database, so a restart resumes | |
| Standby promoted mid-rotation | it holds `next` too (copied) and continues | the copy carries rotation state |
| Mixed fleet | old agents never get the announcement | they keep working on the old cert until retirement, so retirement is blocked while any such agent is registered, unless forced |

## Why it is not built now

It changes the agent (new message, second pin storage, new feature flag), the standby copy format, the relay gateway record and the operator tooling, and the fleet-wide rollout order matters (agents first, then the manager can announce). Doing it without a real multi-machine rollback test would put the whole fleet one mistake away from being locked out, which is worse than the manual re-pin it replaces.

## Until then

Manual re-pin: stop the manager, replace `harness-manager-tls/`, start it, and on each device run the join script again (or edit `-manager-fingerprint` / delete the first-use pin file). Keep the certificate's key private and long-lived (the generated one is valid for 10 years) so this is rare.
