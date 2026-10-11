# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/); versions follow
[semver](https://semver.org/). Releases before the first tag are summarised
by milestone from `HANDOVER.md`, which has the detail.

## [Unreleased]

### Added
- Build versioning: `internal/version`, `-version` on `manager`, `agent`,
  `harnessctl` and `relay`, the injected version as the agent's reported
  version in `GET /nodes`, and a public `GET /version` (version, commit,
  platform only).
- CI (build, vet, gofmt, unit and integration tests on Linux/Windows/macOS,
  race detector on Linux, agent cross-compile), nightly flake hunt,
  govulncheck / dependency review / CodeQL, tag-driven signed releases and
  Dependabot.
- `CONTRIBUTING.md`, `SECURITY.md`, issue and pull request templates,
  `.gitattributes`, `.editorconfig`, `Makefile`.

## Pre-release milestones (2026-09 to 2026-10)

### Foundation (v0 to v5)
- Manager and agents over WebSocket with Ed25519 node identity, pinned
  ECDSA P-256 manager certificate, LAN discovery, heartbeats and
  reconnection.
- Workload execution, resource-aware placement, desired-state
  reconciliation, capability invocation, and a phone as a first-class device.
- Remote access through a self-hosted rendezvous relay, with invitation
  enrolment and approval-based pairing.

### Secure onboarding
- Multi-platform agent catalog and per-platform self-update by binary hash.
- Operator-token authentication on the API, browser guard (Host check and
  CSRF protection), node revocation, fleet hygiene (names, labels,
  duplicate-host detection) and an audit log.

### Real distributed compute
- Capacity reservation, multi-slot nodes and a work queue.
- Content-addressed artifacts; fan-out/fan-in batch jobs with retry.
- Typed task catalog with per-capability policy and dashboard forms.

### Product packaging
- Native Android manager app (foreground service, WebView dashboard, boot
  start); Windows manager bundle with a signed release manifest; Windows
  agent launcher with restart watchdog.
- Single-instance agent lock; manager state export/import.

### AI and wider workloads
- Local models through Ollama; OpenAI-compatible `/v1` API routed to the
  device that has the model; per-device model management; split-model
  inference; AI intent planner behind a deterministic policy check and
  explicit approval.
- Container tasks, media tasks, multi-step workflows, data-local placement
  and priorities; idle- and charging-aware scheduling.
- Standby manager failover, spot-check of results (off by default), remote
  dashboard through the relay (off by default).
