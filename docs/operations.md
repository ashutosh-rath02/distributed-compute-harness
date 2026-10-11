# Operating the manager

## Sizing (measured)

Measured with `cmd/loadsim`: a real `manager.exe` on loopback with TLS, N
simulated devices speaking the real protocol (signed REGISTER, heartbeats every
5 s, assignments answered after 100 ms of simulated work), 3 dashboard event
streams, workloads submitted at a steady rate for 20-25 s. Machine: a Windows 11
laptop (16 threads) shared with other work, so treat the numbers as
order-of-magnitude. Latencies are p50 / p90.

| devices | submit rate | queue wait (submit to device) | submit call | manager CPU avg / max | manager memory peak |
|---|---|---|---|---|---|
| 50 | 50/s | 8 / 149 ms | 15 / 265 ms | 31 % / 41 % of one core | 28 MiB |
| 200 | 50/s | 9 / 92 ms | 16 / 157 ms | 36 % / 50 % | 39 MiB |
| 500 | 50/s | 8 / 251 ms | 15 / 387 ms | 39 % / 75 % | 62 MiB |
| 500 | 200/s offered | 91 / 244 ms | 180 / 498 ms | 75 % / 113 % | 72 MiB |

- **Where it tops out:** about 170 tasks/s sustained at 500 devices (offered
  200/s); at that point the manager is limited by the disk's sync rate and the
  queue wait climbs. Real tasks are far longer than the simulated 100 ms, so
  this is a ceiling on *starts* per second, not on devices.
- **Registration:** 500 devices connecting in the same instant (worst case:
  after a manager restart) are all READY in 4.8 s; some first dials time out
  and retry (jittered). 50 devices: 0.4 s; 200: 1.4 s. A restart with the whole
  fleet connected: 200 devices back in 3.4 s, 500 in 7.9 s.
- **Protocol round trip under load** (PING to PONG): p50 0.5 ms, p99 12-77 ms.
- **Store growth:** the database file grows in 1 MiB-4 MiB steps; 1,000
  workload records plus their audit entries is about 2 MiB. Finished work is
  expired by retention, but the file does not shrink by itself: see compaction.
- **Event streams:** 3 streams at 500 devices cost a few percent of a core;
  each receives every event (about 500/s at 200 submissions/s).
- Memory is tiny. A phone can host the manager for a home fleet; a laptop has
  ample room for hundreds of devices. The practical limit is the disk's fsync
  speed (a spinning disk or a busy SD card will cap throughput far lower), not
  CPU or memory.

Not measured: more than 500 devices (this machine's memory is shared), the
relay path, real agents (heavier than the simulator: each samples the system),
and a phone-hosted manager.

What the load runs found and fixed (before/after compared on the same quiet machine, 500 devices, 50 submissions/s, no profiling): placing an unconstrained task scanned every
workload once per ready device (42 % of CPU in a profile; manager CPU avg 66 % to 42 %, queue wait p50 12 to 5.6 ms, p90 21 to 8 ms after the fix); listing devices did the same; and
every database write waited for its own fsync, so concurrent writers serialized (now
group-committed with a 1 ms wait: a lone write costs 3.1 ms instead of 2.6 ms, but 16 concurrent writers cost 0.22 ms each instead of 7.0 ms each; each write is still durable before it returns).
Still open: registrations are written under one lock, so a
simultaneous herd of 500 takes seconds (one-time); the manager has no profiling
endpoint.

Run it yourself: `go build -o loadsim.exe ./cmd/loadsim`, then
`loadsim -manager path\to\manager.exe -agents 200 -rate 50 -duration 20s -restart`
(ports 18130-18131 by default; it starts and stops its own manager in a temp
folder and never touches a running one).

## Upgrading the manager

1. `harnessctl backup -to pre-upgrade.hhbk -passphrase-file pass.txt` and
   `manager restore -dry-run` it.
2. Install the new build (the installer replaces the binaries and keeps `state\`).
3. On first start the manager upgrades an older database after writing
   `manager.db.pre-upgrade-vN-to-vM-<time>.bak`. A database from a newer
   manager is refused: the log says "was written by a newer manager" and
   nothing is changed. Because the Windows launcher retries every 5 s, the same
   line repeats; install the right build or restore the pre-upgrade backup.
4. A primary and its standby must run the same data format (the standby copies
   the database): upgrade the standby first, then the primary.
5. Keep the pre-upgrade backup until the new version has run a few days.

Note the Windows installer stops the old manager with a hard kill, so the
graceful drain below does not run on an upgrade; in-flight work resumes after
the restart as described under "Manager restart".

## Failure modes and what you see

| What happens | What the operator sees | What the system does |
|---|---|---|
| Manager stopped with Ctrl+C / SIGTERM | log: "shutting down: refusing new work..."; new submissions get 503 | Stops handing out queued work, waits up to `-drain-timeout` (10 s) for running work, closes every device connection, flushes the store, exits (worst case about 20 s). A second signal exits at once. Windows services/tasks that kill the process outright skip the drain. |
| Manager killed or crashed | devices go OFFLINE then READY again on restart | Finished work is kept. Queued work is dispatched after restart, once each. Work that was running is shown UNKNOWN (the device dropped it when the manager vanished); it is never silently re-run. Tested: side effects happen exactly once. |
| Whole fleet reconnects together | brief CPU spike | Devices retry with 50 %-150 % jitter, so they do not arrive in lockstep. |
| Disk full or read-only (database) | log "STORAGE DEGRADED"; submissions get **507** with the reason; `harnessctl backup status` shows it | Reads, dashboards and already-running work continue; nothing refused is half-accepted; a probe every 5 s notices when space returns and normal service resumes by itself, no restart. A failed write never corrupts the database (it is rolled back whole). |
| Disk full (workload files) | uploads get 507 "the manager's disk can't take the file" | Partial uploads are deleted; other work continues. |
| A device's file transfer drops | nothing, usually | Fetches and uploads are retried 3 times; if all fail the task attempt fails and jobs retry it. Hashes are verified before a file appears. |
| Backup fails | log "scheduled backup failed"; `backup status` shows the error | Older backups are kept; a bad new one is deleted. |
| Database from a newer version | startup refused with a clear message | Nothing changed. |
| Clock jumps | none | Heartbeat expiry uses the monotonic clock, unaffected. Only time-based retention and artifact garbage collection read wall time; a clock set back never deletes recent results, a clock set forward expires results early (by design of retention). |

Disk-full and read-only handling is tested by injecting write errors at the
store and artifact layers (a real full disk cannot be produced portably here
without administrator rights); the error classification covers Linux
ENOSPC/EROFS/EDQUOT and Windows ERROR_DISK_FULL/WRITE_PROTECT but was not
exercised against a real full volume.

## Housekeeping

- **Compaction.** bbolt never shrinks the file after deletes.
  `manager compact -db manager.db` (manager stopped) rewrites it without free
  pages, checks the copy, and swaps it in; the original is untouched if
  anything fails. `-compact-db-over 256MiB` does this at start when the file
  is larger. It is offline only on purpose: swapping the file under a live
  store would pull it from under every writer at once.
- **Artifact garbage collection** runs hourly and removes stored files no live
  work needs after `-artifact-retention` (7 days). `harnessctl backup status`
  shows the last pass and totals.
- **Backups** are described in [backup-restore.md](backup-restore.md).

## Test tools

- `cmd/loadsim`: the load run above.
- `test/integration/chaos_test.go`: manager killed with queued, running and
  finished work (each side effect exactly once, queued work done within 30 s);
  connections cut mid-download (output hash equals input hash); 24 agents
  reconnecting at once (back within 20 s); a slow link cut every 0.7 s (no
  completed task has a duplicate side effect; new work completes afterwards);
  a full disk (507, reads continue, recovery without restart, accepted data
  durable); a clean drain.
