# Home Compute Harness — Visual Workflow Guide

## What it does

The Android phone coordinates work. Connected computers supply the CPU, memory, and specialized runtimes.

```mermaid
flowchart LR
    U[User] --> P[Phone manager]
    P -->|Select node| W1[Desktop agent 1]
    P -->|Select node| W2[Desktop agent 2]
    W1 --> C1[Local computation]
    W2 --> C2[Local computation]
    C1 -->|Result| P
    C2 -->|Result| P
```

> The phone makes decisions; the selected desktop performs the computation.

## System layers

```mermaid
flowchart TB
    UI[Phone dashboard<br/>Nodes, tasks, results]
    API[Control API<br/>HTTP + SSE]
    M[Manager<br/>Registry, placement, lifecycle]
    D[Domain + protocol<br/>Node, workload, event, envelope]
    T[Transport<br/>TLS WebSocket or relay]
    A[Desktop agent<br/>Resources, execution, reporting]
    OS[Operating system<br/>Actual process or runtime]
    DB[(Persistent state<br/>Nodes and workloads)]

    UI --> API --> M
    M <--> DB
    M --> D --> T --> A --> OS
```

| Layer | Responsibility |
|---|---|
| Dashboard | Request work and observe the system |
| Manager | Decide where work should run |
| Scheduler | Match requirements to a READY node |
| Protocol | Define structured messages |
| Transport | Deliver messages securely |
| Agent | Control work on a remote device |
| OS/runtime | Perform the computation |
| Persistence | Retain identities, states, and results |

## Complete task workflow

```mermaid
sequenceDiagram
    participant U as User / phone UI
    participant M as Phone manager
    participant A as Desktop agent
    participant P as Desktop process

    U->>M: Submit task + requirements
    M->>M: Validate and select node
    M->>A: WORKLOAD_ASSIGN
    A->>P: Start command or capability
    A->>M: RUNNING
    P-->>A: stdout, stderr, exit code
    A->>M: COMPLETED or FAILED
    M->>M: Persist result
    M-->>U: Live event + output
```

```mermaid
stateDiagram-v2
    [*] --> PENDING
    PENDING --> RUNNING: agent starts
    RUNNING --> COMPLETED: success
    RUNNING --> FAILED: execution error
    PENDING --> CANCELED
    RUNNING --> CANCELED
    RUNNING --> UNKNOWN: outcome lost across restart
    COMPLETED --> [*]
    FAILED --> [*]
    CANCELED --> [*]
```

## Communication channels

```mermaid
flowchart LR
    subgraph Phone
        UI[Dashboard]
        M[Manager]
    end
    subgraph Desktop
        A[Agent]
        P[Process]
    end

    UI -->|HTTP requests| M
    M -->|SSE UI events| UI
    A <-->|TLS WebSocket<br/>tasks, status, heartbeat| M
    A <-->|argv, stdout, stderr, exit code| P
```

- **TLS WebSocket:** registration, heartbeats, tasks, cancellation, and results.
- **HTTP:** dashboard actions such as creating and inspecting workloads.
- **SSE:** live manager events sent to the dashboard; it does not carry desktop tasks.
- **Relay:** optional off-LAN carrier; manager TLS remains end to end.

## How tasks are distributed

Each workload may name a specific node or allow automatic placement.

```mermaid
flowchart TD
    R[New workload] --> V[Validate command/capability<br/>and requirements]
    V --> Q{Explicit target?}
    Q -->|Yes| T[Load selected node]
    Q -->|No| L[List READY nodes]
    T --> F
    L --> F[Filter candidates]
    F --> C{Capability available?}
    C -->|No| X[Reject]
    C -->|Yes| M{Enough memory and cores?<br/>CPU below requested maximum?}
    M -->|No| X
    M -->|Yes| S[Choose eligible node]
    S --> P[Persist PENDING state]
    P --> A[Send WORKLOAD_ASSIGN]
```

Current behavior:

- An explicit target stays pinned to that node.
- With no target, the manager chooses an eligible READY node.
- Capability and resource requirements are hard eligibility checks.
- One workload currently runs wholly on one selected node.
- A restart policy may resubmit eligible failed work with backoff.
- Resource checks do not yet reserve capacity, so simultaneous placements can oversubscribe a node.

### Scaling independent work

```mermaid
flowchart LR
    B[Large batch] --> S[Split into independent tasks]
    S --> T1[Task A]
    S --> T2[Task B]
    S --> T3[Task C]
    T1 --> N1[Desktop 1]
    T2 --> N2[Desktop 2]
    T3 --> N3[Desktop 3]
    N1 --> G[Collect results]
    N2 --> G
    N3 --> G
```

Good fits: image batches, document partitions, builds, tests, embeddings, evaluations, and independent LLM prompts.

### What is not automatic

```mermaid
flowchart LR
    M[One very large model/process] --> H{Fits one node?}
    H -->|Yes| N[Run on selected node]
    H -->|No| D[Use a specialized distributed runtime]
    D --> R[Model sharding, pipeline<br/>or data parallelism]
```

The harness can start and coordinate a distributed runtime, but that runtime must split the computation and exchange its data.

## Device enrollment

```mermaid
sequenceDiagram
    participant P as Phone
    participant D as New desktop
    participant M as Manager

    P->>M: Create 10-minute invitation
    M-->>P: QR + one-time link
    P-->>D: Share link
    D->>M: Download agent
    D->>D: Verify binary hash
    D->>M: REGISTER(token, public key, signature)
    M->>M: Consume token
    M-->>D: Accepted
    D->>M: Heartbeats using persistent identity
    M-->>P: Node READY
```

The invitation is used only for first admission. Reconnects authenticate with the device’s persistent cryptographic identity.

## Edge cases and current handling

| Edge case | Current behavior |
|---|---|
| No READY node | Workload is rejected instead of waiting indefinitely |
| Insufficient memory/cores | Candidate is excluded; request fails if none qualify |
| Required capability missing | Node is excluded from placement |
| Explicit target offline | Request is rejected; it is not silently moved elsewhere |
| Agent loses connection | Heartbeat expiry marks it OFFLINE and in-flight work is failed |
| Agent reconnects | Persistent identity restores the known node without reusing enrollment |
| Manager restarts during work | Persisted in-flight status becomes `UNKNOWN` when the real outcome cannot be proven |
| User cancels running work | Manager sends cancellation; agent terminates the local workload |
| Process exits non-zero | Workload becomes FAILED with stderr, exit code, and error context |
| Excessive output | Captured stdout/stderr is capped at 64 KiB and marked truncated |
| Wrong OS agent binary | QR enrollment rejects the platform mismatch |
| Expired/reused invitation | Enrollment returns not found and cannot admit another identity |
| Phone Wi-Fi IP changes | LAN agents rediscover the manager; networks that block multicast still need a stable address or relay |
| Duplicate agent identities | They appear as separate nodes and may overstate physical capacity; host deduplication is not implemented yet |
| Two tasks select the same free resources | Capacity is not reserved yet, so oversubscription is possible |
| Relay becomes unavailable | Agent reconnect loop uses exponential backoff; LAN path remains independent when configured |
| Manager certificate changes | Pinned clients reject it until the new fingerprint is explicitly trusted |
| Insecure agent mode | Agent refuses workload execution to avoid arbitrary code from an unauthenticated manager |

## Failure and recovery

```mermaid
flowchart TD
    C[Connection lost] --> H{Heartbeats return?}
    H -->|Yes| R[Persistent identity reconnects]
    H -->|No| O[Manager marks node OFFLINE]
    O --> W[Resolve in-flight work as failed<br/>or unknown after restart]
    R --> Y[Node returns to READY]
```

## Compact mental model

```text
Phone             = coordinator
Manager           = decision maker
Scheduler         = node selector
TLS WebSocket     = secure task/result channel
Agent             = worker controller
Desktop process   = actual computation
SSE               = live dashboard notification
Persistent store  = history and recovery
```
