# Schedules

A schedule submits a job or a workflow again and again: on a cron expression, an `@macro`, or `every 15m`. Schedules are stored in the manager's database, survive restarts, and are evaluated by the manager's reconcile loop. Every run goes through the same checks as a hand-made submission.

```
harnessctl schedule add -cron "0 3 * * *" -tz Europe/Berlin -name nightly -workflow nightly.json
harnessctl schedule add -cron "every 15m" -job probe.json -overlap skip -max-runs 96
harnessctl schedule list
harnessctl schedule show <id>          # history: last 50 runs, outcome, links to the job/workflow
harnessctl schedule pause|resume|run-now|delete <id>
```

`nightly.json` is a workflow as for `harnessctl workflow FILE.json` (its `files` are uploaded when the schedule is made and kept for it); `probe.json` is a job as for `POST /jobs`. API: `POST /schedules`, `GET /schedules`, `GET /schedules/{id}`, `POST /schedules/{id}/pause|resume|run`, `DELETE /schedules/{id}`, all with the operator credentials.

## Expressions

- Five cron fields: minute hour day-of-month month day-of-week. Lists (`1,15`), ranges (`9-17`), steps (`*/15`, `10-50/10`), names (`jan`, `mon-fri`); Sunday is 0 or 7.
- `@hourly @daily @midnight @weekly @monthly @yearly @annually`.
- `every 15m`, `every 2h`, `every 1d`, `every 1h30m` (1 minute to 366 days), counted from when the schedule was made.
- Day-of-month and day-of-week: when both are restricted, a day matches if either does; when one is `*`, only the other counts.
- `-tz` is an IANA zone (`Europe/Berlin`); the default is UTC. `Local` is refused: it would mean something else on another machine. Zone data is built into the manager. An expression that never matches (`0 0 30 2 *`) is refused.
- Daylight saving: a wall time the zone skips (02:30 on the spring-forward day) runs once, right after the jump (as 03:30). A repeated hour (fall back) runs on its first pass only, never twice. `every` ignores zones.

## Policies

| flag | values | meaning |
|---|---|---|
| `-missed` | `skip` (default), `run-once` | slots that passed while the manager was off (or the clock jumped over them): dropped, or one catch-up run however many were missed. A slot picked up within 90 s of its time is on time and always runs. |
| `-overlap` | `skip` (default), `queue`, `allow` | a slot arrives while the run before it (job or workflow) is still going: dropped, one waits for it to finish (further ones are dropped), or both run |
| `-max-runs N` | | the schedule ENDS after N runs (failed attempts count; manual runs do not) |
| `-end TIME` | RFC 3339 | no slot after this instant runs; the schedule then ENDS |

States: ACTIVE, PAUSED, ENDED. Resuming does not catch up the paused time. `run-now` starts one run immediately whatever the state or overlap policy; it does not move the schedule or count toward `-max-runs`.

## Exactly once

Each run is tagged with its slot (the scheduled instant). The job or workflow record is written before the schedule's own record (cursor, run count, history, queued slot: one write). If the manager dies between the two, the next pass finds the run by its tag, links it and moves on instead of submitting again. No slot at or before the last one dealt with ever fires, so a clock that jumps backwards cannot repeat a run (the schedule waits until the clock passes the next slot), and one that jumps forwards runs only the latest slot (older ones follow `-missed`). Only wall-clock time is used, never the monotonic reading.

## Run-time checks

A run is submitted with `SubmitJob`/`SubmitWorkflow`: policy, typed parameters, files present, a device able to run it. While no device at all is ready (the manager has just restarted and agents are reconnecting), an on-time slot (or a `run-once` catch-up) waits for the next pass rather than being used up; a slot dropped as missed meanwhile follows `-missed`. Any other refusal (a type disabled since, a deleted file, no device able to run it) is recorded as a failed run with the reason, counts as a run, and is not retried: the slot is used up. It never bypasses a check. Creating a schedule runs the same checks, so a bad one is refused at once.

## Files

The artifacts a schedule's job or workflow names are protected from garbage collection and from `DELETE /artifacts/{sha}` for as long as the schedule exists (paused included; not once ENDED or deleted).

## Audit

`schedule.created`, `schedule.paused`, `schedule.resumed`, `schedule.deleted` and manual `schedule.run` go to the security log with the operator as actor; automatic runs are `schedule.run` entries in the noise log with actor `scheduler`.
