# Home Compute Harness — Baseline Architecture

**Status:** Frozen baseline  
**Purpose:** Common reference for all future design and implementation decisions.

## Core Idea

Treat all participating hardware as one programmable resource fabric. A device joins the fabric by describing its **identity, resources, capabilities, state, and supported execution/communication methods**. The harness decides how those resources are discovered, governed, and used.

## 1. Architectural Goal

Build a **device-agnostic harness** that can start with a few computers on a home network and later expand to phones, edge devices, sensors, cloud machines, accelerators, and future hardware without redesigning the core.

The architecture is **not tied to Wi-Fi, Kubernetes, containers, a specific OS, or a specific device category**.

## 2. Reference Architecture

```text
┌──────────────────────────────────────────────────────────────┐
│                       INTENT / API LAYER                     │
│  Apps, automation, and future AI planners express intent    │
├──────────────────────────────────────────────────────────────┤
│                         CONTROL PLANE                        │
│  Resource Graph │ Discovery │ Identity │ State │ Events     │
│  Policy │ Lifecycle │ Scheduling │ Service Registry         │
├──────────────────────────────────────────────────────────────┤
│                        EXTENSION LAYER                       │
│  Device Adapters │ Transports │ Runtimes │ Capabilities     │
│  Storage │ Scheduler Extensions                            │
├──────────────────────────────────────────────────────────────┤
│                    EXECUTION & DATA PLANE                    │
│  Laptops │ Desktops │ Phones │ NAS │ Edge/IoT │ Cloud      │
└──────────────────────────────────────────────────────────────┘
```

The **control plane coordinates**. Execution and bulk data should not be forced through the manager; nodes may communicate directly when policy allows.

## 3. Core Domain Model

| Concept | Meaning | Examples |
|---|---|---|
| **Node** | Anything that participates in the fabric; not necessarily a physical device | Laptop, phone, cloud VM, gateway, cluster |
| **Resource** | Consumable capacity exposed by a node | CPU, RAM, storage, GPU, bandwidth |
| **Capability** | An action or function a node can provide | Camera capture, inference, Bluetooth scan, sensor read |
| **Workload** | A desired unit of work | Job, service, capability invocation |
| **Runtime** | How a workload executes | WASM, container, native, Python, platform runtime |
| **Transport** | How harness messages/data move | LAN, QUIC/WebSocket, mesh, relay, BLE gateway |
| **Policy** | Rules constraining resource/capability usage | Local-only, charging-only, privacy, priority |
| **Event** | A state change emitted by the harness | `node.joined`, `node.offline`, `workload.completed` |

## 4. Node Contract

Every participating node must expose the same logical contract regardless of platform:

- **Identity** — persistent identity independent of IP address or current network.
- **Resources** — capacity the node can provide.
- **Capabilities** — actions/functions the node can expose.
- **State** — health, availability, and dynamic operating conditions.
- **Runtimes** — supported workload execution mechanisms.
- **Transport** — one or more ways the node communicates with the fabric.

**Key rule:** core services and schedulers operate on these contracts, not device-specific logic such as `if Android` or `if Windows`.

## 5. Communication & Identity

- **Physical connectivity:** Wi-Fi, Ethernet, Internet, Bluetooth gateways, or future transports.
- **Logical connectivity:** the stable layer is the **Harness Protocol + cryptographic node identity**.
- **Discovery:** pluggable; local discovery first, with mesh, relay, manual pairing, or QR-based flows possible later.
- **Security:** nodes are explicitly trusted and mutually authenticated. Being on the same network must never imply execution authority.

## 6. Control Plane Responsibilities

- **Resource Graph** — current view of nodes, resources, and capabilities.
- **State Store** — separates persistent identity/configuration from fast-changing runtime state.
- **Event Bus** — exposes node, resource, and workload lifecycle changes without tight coupling.
- **Scheduler** — chooses eligible nodes based on requirements, resource fit, and policy.
- **Lifecycle / Reconciliation** — manages finite jobs and desired-state services.
- **Policy Engine** — enforces security, privacy, battery, locality, cost, and usage constraints.

## 7. Extension Model

New functionality should enter through explicit extension points rather than modifications to the core:

- **Device adapters** — Android, iOS, TVs, vehicles, robots, appliances.
- **Runtime plugins** — WASM, containers, native executors, accelerators.
- **Transport plugins** — LAN, secure mesh, Internet relay, Bluetooth/IoT gateways.
- **Capability plugins** — cameras, sensors, microphones, Bluetooth, AI inference, storage.
- **Scheduler / policy plugins** — battery-aware, latency-aware, energy-aware, cost-aware, locality-aware.
- **Storage / service integrations** — NAS, databases, cloud object stores, external orchestrators.

## 8. Design Rules — Source of Truth

1. **Device-agnostic core** — the core must not require knowledge of a specific device, OS, cloud provider, runtime, or network technology.
2. **Capability-first modeling** — a node is valuable for what it exposes, not what category it belongs to.
3. **Control/data separation** — the manager coordinates; bulk workload data should move directly between endpoints where possible.
4. **Churn is normal** — devices may sleep, move networks, throttle, or disappear; identity, heartbeats, leases, and reconciliation must assume this.
5. **Deterministic enforcement** — AI may propose plans, but security, policy, and execution guarantees are enforced by deterministic harness components.
6. **Versioned contracts** — protocol messages, manifests, and extension contracts must be versioned.
7. **Start centralized, allow evolution** — v0 may use one manager, but interfaces and state must not block a future distributed/high-availability control plane.

## 9. Baseline Evolution Path

| Stage | Focus |
|---|---|
| **v0** | Connectivity, identity, manifests, communication |
| **v1** | Workload execution and basic scheduling |
| **v2** | Services, policy, richer runtimes and capabilities |
| **Later** | Phones/IoT, mesh networking, cloud integration, AI intent planning |

---

This document defines the architectural baseline. Implementation choices may change as long as they preserve these **contracts, boundaries, and design rules**.
