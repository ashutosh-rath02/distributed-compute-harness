# @home-harness/client (JavaScript)

A client for the Home Harness operator API. Node 20.3 or newer, ES modules, global
`fetch`, no dependencies. The API is described in `docs/openapi.yaml`.

```js
import { Client } from '@home-harness/client';   // or: import from './clients/js/src/index.js'
import { readFileSync } from 'node:fs';

// The token is in the manager's harness-operator-token file. Keep it out of source code.
const hh = new Client('http://127.0.0.1:7421', readFileSync('harness-operator-token', 'utf8').trim());
// or: Client.fromEnv()  (HARNESS_OPERATOR_TOKEN, optionally HARNESS_URL)

for (const n of await hh.nodes()) console.log(n.name, n.state);
```

## Run a task and get its files

```js
const ref = await hh.uploadArtifact('notes.txt');                       // { name, sha256, size }
const work = await hh.submitTask('file.hash', { algorithm: 'sha256' }, { inputs: [ref] });
const done = await hh.waitWorkload(work.id, { timeoutMs: 120000 });      // polls with backoff
console.log(done.state);                                                // COMPLETED / FAILED / CANCELED
await hh.downloadOutputs(done, 'results/');
```

`hh.catalog()` lists the task types and their parameters. Parameter values may be
numbers or booleans; they are sent as strings, as the API expects.

## Jobs, workflows, plans

```js
const job = await hh.submitJob([{ capability: 'file.hash', inputs: [a] }, { capability: 'file.hash', inputs: [b] }], { maxAttempts: 3 });
await hh.waitJob(job.id);

const wf = await hh.submitWorkflow(
  [{ type: 'image.resize', params: { width: 800 }, mode: 'perFile' }, { type: 'archive.zip', params: { name: 'small.zip' }, mode: 'once' }],
  { inputs: [photo1, photo2] });
await hh.downloadOutputs(await hh.waitWorkflow(wf.id), 'out/');

const { models } = await hh.plans();
let plan = await hh.waitPlan((await hh.plan('Make thumbnails and zip them', models[0], [photo1])).id);
if (plan.state === 'proposed') {
  console.log(plan.summary);            // read it first
  await hh.approvePlan(plan.id);        // only now does anything run
}
```

## Events

```js
const ac = new AbortController();
for await (const ev of hh.events({ signal: ac.signal })) console.log(ev.type, ev.nodeId);
```

There is no replay and a slow reader can miss events: treat an event as a hint to re-read state.

## The fleet's models from the OpenAI library

```js
import OpenAI from 'openai';
const client = new OpenAI(await hh.openaiConfig());   // { baseURL, apiKey }
```

The AI key opens only the `/v1` routes.

## Errors

Failures reject with `HarnessError` (`.status`, `.body` raw text, `.detail` the message for a person).
Subclasses: `AuthenticationError` (401), `PolicyError` (403), `NotFoundError` (404), `ConflictError` (409,
nowhere to run it right now), `WaitTimeout` (the work may still be running). `.status` is 0 when the manager
could not be reached.

## Tests

The tests start a throwaway manager and agent on loopback ports from `HH_TEST_PORT` (default 18163, uses
three), with all state in a temporary folder.

```
HH_MANAGER_BIN=/tmp/hh/manager HH_AGENT_BIN=/tmp/hh/agent node --test clients/js
```

Without the binaries only the offline tests run.
