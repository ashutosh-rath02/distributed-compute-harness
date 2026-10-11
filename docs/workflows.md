# Workflows: the stage spec

A workflow is 1-8 stages that run in order. Each stage is an ordinary batch job built from a task type in the catalog (`harnessctl tasks`), working on the files the stages before it made. The whole workflow is checked before anything runs: every stage's types, parameters, file names and counts, policy, and whether some device could run it.

Submit with `harnessctl workflow FILE.json`, `harnessctl workflow -step ...`, or `POST /workflows`.

```json
{
  "name": "photos",
  "maxAttempts": 3,
  "inputs": [{"name": "a.jpg", "sha256": "..."}],
  "stages": [ { ...stage... }, { ...stage... } ]
}
```

## A stage

| field | meaning |
|---|---|
| `type` | catalog task type (required). Raw commands and device-bound types are refused. |
| `params` | the type's parameters, as strings |
| `mode` | `once` (default: one run with every file), `perFile` (one run per file, spread over the fleet), `parts` (N runs of a type that splits, e.g. `render.fractal`) |
| `parts` | with mode `parts`: how many |
| `combine` | `{type, params}`: a fan-in step over all of the stage's results (the job's reduce) |
| `name` | label |
| `when` | run only if this condition holds (below). Empty = always. |
| `onFailure` | `stop` (default), `continue`, or `retry:N` (N = 1-5) |
| `timeoutSeconds` | limit for each run of each task (1-86400). Never raises the operator policy's limit. Needs agents with `timeout.v1`; placement only uses those. |
| `maxAttempts` | attempts per task for this stage (1-10), instead of the workflow's |

All new fields are optional; a workflow without them behaves exactly as before.

## What a stage is given

A stage works on the results of **the nearest earlier stage that completed**, or on the workflow's own `inputs` if none did. A stage that was skipped, or that failed and was continued past, is passed over. So a skipped stage never breaks the stage after it.

Because that depends on how the run goes, submission checks a stage against **every** set of files it could be given (the stage before it, and, while those stages are optional, the ones before them, back to the workflow's files). If the stage cannot take one of them, the workflow is refused up front, saying which step's skip or failure causes it. A stage after the first always needs files to work on.

The workflow's results are those of the last stage that completed.

## Conditions (`when`)

A small closed language, not a scripting language: no variables, no arithmetic, no loops. It is parsed by hand with limits (512 bytes, 64 parts, 8 deep) and checked at submission; evaluating it cannot fail.

```
expr    = or
or      = and { "or" and }
and     = not { "and" not }
not     = "not" not | primary
primary = "(" expr ")" | "true" | "false"
        | stage.succeeded | stage.failed | stage.skipped
        | stage.state ( "==" | "!=" ) "COMPLETED" | "FAILED" | "SKIPPED"
        | ( stage.outputs | inputs ) ( == != >= <= > < ) NUMBER
        | ( stage.outputs | inputs ) .any(name matches "GLOB")
stage   = previous | stage[K]
```

- `previous` is the stage right before this one in the list (whatever happened to it); `stage[K]` is step K counted from 1. A stage can only refer to stages before it.
- `outputs` is how many files that stage handed on (0 unless it completed); `inputs` is the number of files the workflow was given.
- `GLOB` is `path.Match` syntax (`*.png`, `chunk-????.txt`), tried against each file's whole name and its last path element.
- `and` binds tighter than `or`.

Examples: `previous.failed`, `previous.succeeded and previous.outputs >= 2`, `inputs.any(name matches "*.png") or stage[1].skipped`, `not (stage[2].state == "COMPLETED")`.

## Failure policy

- `stop` (default): the stage's job fails (a task ran out of attempts) and the workflow fails with that reason. Later stages show SKIPPED, "the workflow ended before it ran".
- `continue`: the stage may fail. It shows FAILED, later stages see it as `previous.failed`, and the workflow carries on. A stage that cannot even start (a type disabled since, nothing to work on) is also tolerated and recorded.
- `retry:N`: when the stage's job fails, a new job for the same stage is submitted, up to N more times (each re-runs every task, each with its own `maxAttempts`). When the retries are used up it fails like `stop`. Only a failure the stage can recover from is retried; one that can never succeed (policy) is not.

A workflow is COMPLETED when every stage is COMPLETED, SKIPPED or a tolerated FAILED. A stage's state is one of PENDING, WAITING, RUNNING, COMPLETED, FAILED, CANCELED, SKIPPED; SKIPPED and job-less FAILED carry a `reason`.

## Restarts

Where a workflow stands is derived from the stages' jobs, never stored as a counter. A stage that has a job is never started again; skip decisions are recomputed from the earlier stages' results, so a manager restarted at any point reaches the same decisions. Retries are counted from the jobs that exist. The only extra stored fact is a tolerated failure that never got a job. The chaos test (`test/integration/schedules_test.go`) restarts the manager mid-stage and between scheduled ticks and checks one job per stage per run.

## Example

Resize photos; zip them only if there are at least two; if the resize fails, checksum the originals instead:

```json
{
  "files": ["photos/*.jpg"],
  "stages": [
    {"type": "image.resize", "mode": "perFile", "params": {"width": 800}, "onFailure": "continue", "timeoutSeconds": 120},
    {"type": "archive.zip", "params": {"name": "small.zip"}, "when": "previous.succeeded and previous.outputs >= 2"},
    {"type": "file.hash", "when": "stage[1].failed"}
  ]
}
```

Map-reduce workflows (`text.split`, `json.merge`) are built by `harnessctl mapreduce`; see `docs/examples/wordcount.md`.
