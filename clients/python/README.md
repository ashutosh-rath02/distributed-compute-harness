# homeharness (Python)

A client for the Home Harness operator API. Python 3.9 or newer, standard
library only. The API is described in `docs/openapi.yaml`.

```
pip install ./clients/python          # from a checkout
pip install "./clients/python[openai]"  # if you also want openai_client()
```

## Connect

The token is the contents of the manager's `harness-operator-token` file. Keep
it out of source code.

```python
import os
from homeharness import Client

hh = Client("http://127.0.0.1:7421", open("harness-operator-token").read().strip())
# or: set HARNESS_OPERATOR_TOKEN (and optionally HARNESS_URL) and use Client.from_env()

for node in hh.nodes():
    print(node["name"], node["state"], node["platform"]["os"])
```

## Run a task and get its files

```python
ref = hh.upload_artifact("notes.txt")                         # {"name", "sha256", "size"}
work = hh.submit_task("file.hash", {"algorithm": "sha256"}, inputs=[ref])
done = hh.wait_workload(work["id"], timeout=120)              # polls with backoff
print(done["state"])                                          # COMPLETED / FAILED / CANCELED
hh.download_outputs(done, "results/")                         # files the task produced
```

`hh.catalog()` lists the task types and their parameters. Parameter values may
be numbers or booleans; they are sent as strings, as the API expects.

## Jobs, workflows, plans

```python
job = hh.submit_job(
    [{"capability": "file.hash", "inputs": [a]}, {"capability": "file.hash", "inputs": [b]}],
    max_attempts=3,
)
hh.wait_job(job["id"])

wf = hh.submit_workflow(
    [{"type": "image.resize", "params": {"width": 800}, "mode": "perFile"},
     {"type": "archive.zip", "params": {"name": "small.zip"}, "mode": "once"}],
    inputs=[photo1, photo2],
)
result = hh.wait_workflow(wf["id"])
hh.download_outputs(result, "out/")

plan = hh.plan("Make thumbnails of these photos and zip them", model=hh.plans()["models"][0], files=[photo1])
plan = hh.wait_plan(plan["id"])
if plan["state"] == "proposed":
    print(plan["summary"])          # read it first
    hh.approve_plan(plan["id"])     # only now does anything run
```

## Events

```python
for event in hh.events():
    print(event["type"], event["nodeId"])
```

There is no replay, and a slow reader can miss events: treat an event as a hint
to re-read state.

## Use the fleet's models from the OpenAI library

```python
client = hh.openai_client()        # needs: pip install openai
reply = client.chat.completions.create(model=hh.models()[0], messages=[{"role": "user", "content": "Hello"}])
```

`hh.openai_config()` returns `{"base_url", "api_key"}` for any other library.
The AI key opens only the `/v1` routes.

## Errors

Every failure is a `HarnessError` with `.status`, `.body` (the raw text) and
`.message` (what to show a person). Subclasses: `AuthenticationError` (401),
`PolicyError` (403, the policy does not allow it), `NotFoundError` (404),
`ConflictError` (409, nowhere to run it right now), and `WaitTimeout` (a
`wait_*` call ran out of time; the work may still be running). `.status` is 0
when the manager could not be reached.

## Tests

The tests start a throwaway manager and agent on loopback ports from
`HH_TEST_PORT` (default 18160, uses three) with all state in a temporary
folder.

```
go build -o /tmp/hh/manager ./cmd/manager && go build -o /tmp/hh/agent ./cmd/agent
HH_MANAGER_BIN=/tmp/hh/manager HH_AGENT_BIN=/tmp/hh/agent python -m unittest discover -s clients/python/tests
```

Without the binaries only the offline tests run.
