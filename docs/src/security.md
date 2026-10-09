# Security overview

Home Harness is built for a home or small-office network where the devices are yours or belong to people you know. It is not built to run code from strangers. This page says what it protects, and how.

For the full analysis see the [threat model](threat-model.md) (written separately; this page is the short version).

## The rule everything follows

**The manager never trusts agents, AI models or relays.** An agent can lie about what it has or what it did; the manager checks reports against what it asked for, never runs anything an agent sends, and treats every agent-supplied name or text as untrusted data. The same goes for the output of an AI model and for a relay in the middle.

## How devices are authenticated

- Agents and the manager talk over **mutually authenticated TLS**. The manager's certificate is created on first start (ECDSA P-256). Agents **pin its SHA-256 fingerprint**, so a device on your network cannot pose as your manager.
- A device joins either with the **pairing token** (a long random secret) or **by approval**: you compare a six-digit code on the device and on the dashboard. The code is computed on each side from the manager's certificate and the device's key, so someone in the middle makes the two codes differ.
- Each device has its own **identity** (a key pair). You can **revoke** it: the manager disconnects it and refuses it again even with a valid token.
- The join page is plain HTTP on purpose, so devices need not click through certificate warnings. It does **not** protect the downloads against someone who controls your network at install time. Add devices on a network you trust, and keep the join window closed otherwise (it closes by itself).

## How you are authenticated

- Every operator API route needs the **operator token** as a Bearer header, except the dashboard's static page and a few bootstrap routes. The token is a file in the manager's private state folder.
- The API listens on **loopback** only, and answers only requests addressed to an IP or `localhost`, which defeats browser-based DNS-rebinding and cross-site request tricks from web pages you visit.
- **No cookies, ever.** Cookies are shared across ports on the same host, so a cookie would leak to other apps listening on other local ports. The dashboard keeps a derived session value in `localStorage` and sends it as a header. It loads no external scripts and escapes everything agents or models send.
- The **AI key** opens only the OpenAI-compatible routes. A chat app holding it cannot submit tasks, change policy, or approve an AI plan.

## What a task can do

By default only the **typed catalog** runs: each task is implemented in the agent, validated by both manager and agent, confined to its own working folder, and has a fixed parameter list. **Raw commands and host file reads are off** until you enable them in the policy; if you do, restrict them to specific devices with labels.

Container tasks (`container.run`) are also off by default, and when on run only the images you list, preferably pinned by digest, without network unless you allow it.

For devices you do not fully control, enable **spot checks**: the manager re-runs a share of deterministic tasks on a different device and compares the results, marking a device whose answers differ.

## Your data

State lives in one folder on the manager: the database, the operator token, the certificate and its key, and stored files. Protect it like a password manager. Back it up with the manager's `-export-state`, and keep the export private: it holds the key and tokens. Removing a manager's state is permanent for the devices that trusted it.

## What it does not protect against

- Someone with the operator token or the manager's state folder has full control.
- A malicious program you run through a raw command or an opted-in container runs with that agent's user rights.
- A device that is physically yours to lose can reveal its identity to whoever has it: revoke it.
- Denial of service by a device you admitted is limited (rate limits, caps) but not impossible.

## Reporting

Please report security problems privately to the maintainers rather than in a public issue.
