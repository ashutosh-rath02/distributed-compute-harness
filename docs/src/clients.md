# Client libraries

Both libraries are small, have no dependencies beyond the language's standard library, and cover the same ground: nodes, tasks, jobs, workflows, AI plans, file upload and download, waiting for completion with backoff, the event stream, and the OpenAI-compatible address. They live in the repository under `clients/`, and the [HTTP API](api.md) is described in [openapi.yaml](openapi.yaml) if you would rather use another language.

You need the manager's **operator token** (the contents of its `harness-operator-token` file). Keep it out of source code: read it from a file or an environment variable.

## Python (3.9 or newer)

```
pip install ./clients/python
```

```python
from homeharness import Client

hh = Client.from_env()          # HARNESS_OPERATOR_TOKEN, optional HARNESS_URL
ref = hh.upload_artifact("notes.txt")
work = hh.submit_task("file.hash", {"algorithm": "sha256"}, inputs=[ref])
done = hh.wait_workload(work["id"])
hh.download_outputs(done, "results/")
```

## JavaScript (Node 20 or newer)

```js
import { Client } from './clients/js/src/index.js';

const hh = Client.fromEnv();
const ref = await hh.uploadArtifact('notes.txt');
const work = await hh.submitTask('file.hash', { algorithm: 'sha256' }, { inputs: [ref] });
const done = await hh.waitWorkload(work.id);
await hh.downloadOutputs(done, 'results/');
```

## Errors

A failed call raises `HarnessError` with the HTTP status, the raw body and a message meant for a person. There are subclasses for the common cases: wrong token (401), not allowed by policy (403), not found (404), and nowhere to run it right now (409).

## The fleet's AI models from the OpenAI libraries

`openai_client()` (Python) and `openaiConfig()` (JavaScript) give you the base URL and AI key of your manager's OpenAI-compatible API, so existing apps can use your own devices' models.

The full READMEs, with more examples, are in `clients/python/README.md` and `clients/js/README.md`.
