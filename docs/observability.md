# Observability

What the manager tells you about itself: Prometheus metrics, health probes,
structured logs, a one-screen status, and a Health card in the dashboard.

## Endpoints and who may call them

| Route | Auth | Answer |
|---|---|---|
| `GET /metrics` | operator token **or** the metrics token | Prometheus text format 0.0.4 |
| `GET /healthz` | none | `ok` (200), or `stalled` (503) if the manager's reconcile loop has not ticked for `6 x -reconcile-interval + 30 s` |
| `GET /readyz` | none | `ready` (200), or `not ready: <reason>` (503). Reasons: `stepped down`, `not listening`, `store not writable` |
| `GET /status` | operator token only | JSON summary used by `harnessctl status` / `top` and the dashboard Health card |

`/healthz` and `/readyz` are unauthenticated on purpose: an orchestrator or load
balancer must be able to ask without a secret, and the bodies are a fixed word.
They reveal nothing beyond "this is a harness manager and it is (not) ready".
A manager running as a standby answers `/healthz` ok and `/readyz` 503
`not ready: standby`. Every other route keeps its existing authentication.

`/readyz` checks that the database can take a write by opening a write
transaction and rolling it back (no disk sync, and the standby's "up to date"
check is not disturbed); the result is cached for 5 seconds.

### Giving Prometheus its own token

The operator token can do everything. Do not hand it to a scraper. Start the
manager with

    manager ... -metrics-token-file harness-metrics-token

The file is created (owner-only) on first start if missing. That token is
accepted **only** on `GET /metrics`, `/healthz` and `/readyz`: on any other
route it is refused like a wrong token, and the AI key is refused on `/metrics`
too. It must differ from the operator token and the AI key (the manager refuses
to start otherwise). Rotate it by deleting the file and restarting.

### Scrape configuration

    scrape_configs:
      - job_name: home-harness
        metrics_path: /metrics
        scrape_interval: 15s
        authorization:
          type: Bearer
          credentials_file: /etc/prometheus/harness-metrics-token
        static_configs:
          - targets: ["127.0.0.1:7421"]

The operator API listens on loopback by default (`-api-addr`), so run
Prometheus on the same machine (or tunnel to it). Widening `-api-addr` is still
unsupported (see HANDOVER section 11). `docs/alerts.yml` holds alert rules and
`docs/grafana-dashboard.json` a dashboard (import it in Grafana and pick your
Prometheus data source).

## Metrics reference

Labels are bounded by design: capability types, states, route **patterns**
(`GET /nodes/{id}`, never the raw path; unknown paths are `unmatched`),
status classes and code-defined kinds. Never a workload ID, file name or any
text a user or device supplied. Every labelled family also has a series cap
(256, or 400 for HTTP routes) beyond which new combinations fold into one
`other` series. Per-node families (label `node`) show the first 50 nodes by ID.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `harness_build_info` | gauge | version, commit | Always 1 |
| `harness_manager_start_time_seconds` | gauge | | Process start (unix time) |
| `harness_ready` | gauge | | 1 when `/readyz` would say ready |
| `harness_nodes` | gauge | state | Known nodes by state (READY, OFFLINE, CONNECTED, DEGRADED...) |
| `harness_nodes_taking_work` | gauge | | READY devices that accept new work now |
| `harness_nodes_held_back` | gauge | reason | READY devices not taking work: paused, outside_hours, on_battery, in_use, no_report, other |
| `harness_node_heartbeat_age_seconds` | gauge | node | Seconds since each node was heard from |
| `harness_node_heartbeat_age_seconds_max` | gauge | | Same, worst READY node (covers the whole fleet) |
| `harness_node_slots`, `harness_node_slots_used` | gauge | node | Workload slots offered / in use |
| `harness_agent_heartbeats_total` | counter | | Heartbeats received |
| `harness_node_registrations_total` | counter | kind | `new` or `reconnect` |
| `harness_node_offline_total` | counter | reason | `heartbeat_timeout` or `connection_closed` |
| `harness_rejections_total` | counter | kind | Refused registrations and sign-ins (`node.rejected`, `operator.login-failed`) |
| `harness_agent_uptime_seconds` | gauge | node | Agent self-report (`self-metrics.v1` agents only) |
| `harness_agent_workloads_run_total`, `_failed_total` | counter | node | Agent self-report: workloads finished / failed since it started |
| `harness_agent_cache_hits_total`, `_cache_misses_total` | counter | node | Agent self-report: input cache lookups |
| `harness_agent_reconnects_total` | counter | node | Agent self-report: reconnects since start |
| `harness_workloads` | gauge | state | Workloads held, by state (QUEUED, PENDING, RUNNING, COMPLETED, FAILED, CANCELED, UNKNOWN) |
| `harness_queue_depth` | gauge | | Workloads waiting for a free slot |
| `harness_queue_oldest_wait_seconds` | gauge | | Age of the oldest queued workload |
| `harness_workload_queue_wait_seconds` | histogram | capability | Accepted-to-handed-to-a-device time (0 when placed at once) |
| `harness_workload_duration_seconds` | histogram | capability, outcome | Run time on the device |
| `harness_workloads_finished_total` | counter | capability, outcome | completed / failed / canceled |
| `harness_workload_timeouts_total` | counter | capability | Failures from the time limit |
| `harness_workload_retries_total` | counter | reason | `busy_refusal`, `restart`, `job_attempt` |
| `harness_jobs`, `harness_workflows` | gauge | state | By state |
| `harness_jobs_finished_total`, `harness_workflows_finished_total` | counter | state | Finished, by final state |
| `harness_plans` | gauge | state | AI plans in memory (newest 100) |
| `harness_spot_checks_total` | counter | result | matched, mismatch, unresolved, skipped |
| `harness_spot_checks_open` | gauge | | Checks still being decided |
| `harness_http_requests_total` | counter | route, code | Operator API requests; code is the class (2xx...) |
| `harness_http_request_duration_seconds` | histogram | route | Latency (the event stream is excluded) |
| `harness_event_stream_subscribers` | gauge | | Open `GET /events` streams |
| `harness_artifact_bytes_total` | counter | direction, via | upload/download by operator or agent |
| `harness_artifact_store_used_bytes`, `_limit_bytes` | gauge | | File store usage and limit |
| `harness_store_size_bytes` | gauge | | Database file size |
| `harness_audit_events_total` | counter | log, kind | Audit entries written |
| `harness_failover_term` | gauge | | Failover term (changes when a standby takes over) |
| `harness_failover_stepped_down` | gauge | | 1 when a newer manager superseded this one |
| `harness_standby_configured`, `harness_standby_lag_seconds` | gauge | | Standby present; seconds since it last copied the state |

Counters reset when the manager restarts (Prometheus handles that in
`rate()`/`increase()`). `harness_workloads_finished_total` counts what this
manager process saw finish.

Agent self-metrics are reported by the agent and not verified: they are for
display and monitoring only. Agents older than `self-metrics.v1` simply have no
such series.

## Logs

    manager -log-format text|json -log-level debug|info|warn|error
    agent   -log-format text|json -log-level ...

Text keeps the familiar line and adds fields:

    2026/10/09 12:00:01 workload.completed workload=ab12.. node=node-cd34.. capability=image.resize

JSON is one object per line (`time`, `level`, `msg`, fields). Existing
`log.Printf` lines go through the same handler, so nothing is lost; the
important paths (admission, rejections, workload lifecycle, job and workflow
transitions, failover, update dispatch) are structured with the field names
`node`, `workload`, `job`, `workflow`, `capability`, `reason`.
Never logged: tokens, prompts, request text, file contents, full command
lines. As a safety net, any field named like a secret (token, key, prompt,
argv, command, ...) is written as `[redacted]`.

Every API response carries `X-Request-Id` (generated by the manager, never
taken from the client). Operator actions that write an audit entry include it
as `detail.requestId`, so a response can be traced to its audit line.

## Operator views

    harnessctl status      fleet, queue, recent failures, versions, standby, disk
    harnessctl top         the same, refreshing (-n 2s)
    harnessctl status <id> (unchanged) ask one agent for its status

The dashboard has a **Health** card under the summary tiles.

`GET /nodes/{id}` includes `self` (the agent's own counters) for agents that
report them.
