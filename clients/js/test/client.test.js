// Tests against a throwaway manager and agent. Set HH_MANAGER_BIN and HH_AGENT_BIN
// to built binaries; HH_TEST_PORT is the first of three loopback ports (default 18163).
// Without the binaries only the offline tests run. All state goes in a temp folder.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  Client, HarnessError, AuthenticationError, NotFoundError, PolicyError, WaitTimeout,
} from '../src/index.js';

const MANAGER = process.env.HH_MANAGER_BIN ?? '';
const AGENT = process.env.HH_AGENT_BIN ?? '';
const PORT = Number(process.env.HH_TEST_PORT ?? 18163);
const live = existsSync(MANAGER) && existsSync(AGENT);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

test('a token is required', () => {
  assert.throws(() => new Client('http://127.0.0.1:1', ''), TypeError);
});

test('an unreachable manager is an error with status 0', async () => {
  const c = new Client(`http://127.0.0.1:${PORT + 2}`, 't'.repeat(40), { timeoutMs: 2000 });
  await assert.rejects(c.nodes(), (e) => e instanceof HarnessError && e.status === 0 && /cannot reach the manager/.test(e.message));
});

test('task and stage validation', async () => {
  const c = new Client('http://127.0.0.1:1', 't'.repeat(40));
  await assert.rejects(async () => c.submitJob([{ params: {} }]), TypeError);
  await assert.rejects(async () => c.submitWorkflow([{ params: {} }]), TypeError);
});

const dir = live ? mkdtempSync(join(tmpdir(), 'hh-jsclient-')) : '';
const procs = [];
let hh;

async function waitFor(fn, seconds = 40) {
  const end = Date.now() + seconds * 1000;
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; } catch { /* retry */ }
    await sleep(300);
  }
  throw new Error(`timed out waiting for the throwaway manager/agent (state in ${dir})`);
}

before(async () => {
  if (!live) return;
  const api = `127.0.0.1:${PORT}`;
  const agentAddr = `127.0.0.1:${PORT + 1}`;
  const tokenFile = join(dir, 'operator-token');
  procs.push(spawn(MANAGER, ['-disable-discovery', '-keep-awake=false', '-addr', agentAddr, '-api-addr', api,
    '-pairing-token', 'test-pairing-secret-0123456789', '-db', join(dir, 'm.db'), '-tls-dir', join(dir, 'tls'),
    '-operator-token-file', tokenFile, '-artifact-dir', join(dir, 'art'), '-ai-key-file', join(dir, 'ai-key'),
    '-remote-access-file', join(dir, 'remote'), '-allow-raw-commands'], { cwd: dir, stdio: 'ignore' }));
  await waitFor(async () => existsSync(tokenFile) && (await fetch(`http://${api}/`)).ok);
  hh = new Client(`http://${api}`, readFileSync(tokenFile, 'utf8').trim());
  const { fingerprint } = await hh.request('GET', '/join-info');
  procs.push(spawn(AGENT, ['-manager-fingerprint', fingerprint, '-manager-addr', agentAddr, '-pairing-token', 'test-pairing-secret-0123456789',
    '-identity-dir', join(dir, 'id'), '-work-dir', join(dir, 'work'), '-name', 'js-test-agent', '-keep-awake=false'], { cwd: dir, stdio: 'ignore' }));
  await waitFor(async () => (await hh.readyNodes()).length > 0);
});

after(async () => {
  for (const p of procs) p.kill(); // only the processes this test started
  await sleep(500);
  if (dir) rmSync(dir, { recursive: true, force: true });
});

const t = (name, fn) => test(name, { skip: !live && 'set HH_MANAGER_BIN and HH_AGENT_BIN' }, fn);

t('a wrong token is an AuthenticationError', async () => {
  const bad = new Client(hh.baseUrl, 'x'.repeat(64));
  await assert.rejects(bad.nodes(), (e) => e instanceof AuthenticationError && e.status === 401 && /operator authentication required/.test(e.detail));
});

t('not found keeps status and body', async () => {
  await assert.rejects(hh.workload('no-such-id'), (e) => e instanceof NotFoundError && e.status === 404 && /not found/.test(e.body));
});

t('nodes and catalog', async () => {
  assert.equal((await hh.readyNodes())[0].name, 'js-test-agent');
  const types = (await hh.catalog()).types.map((x) => x.name);
  assert.ok(types.includes('file.hash'));
});

t('upload, run, wait, download', async () => {
  const ref = await hh.uploadArtifact(Buffer.from('hello harness\n'), 'hello.txt');
  assert.equal(ref.sha256.length, 64);
  const w = await hh.submitTask('file.hash', { algorithm: 'sha256' }, { inputs: [ref] });
  const done = await hh.waitWorkload(w.id, { timeoutMs: 60000 });
  assert.equal(done.state, 'COMPLETED', JSON.stringify(done));
  const paths = await hh.downloadOutputs(done, join(dir, 'dl'));
  assert.ok(paths.length > 0);
  assert.match(readFileSync(paths[0], 'utf8'), /hello\.txt/);
});

t('job and workflow', async () => {
  const a = await hh.uploadArtifact(Buffer.from('aaa'), 'a.txt');
  const b = await hh.uploadArtifact(Buffer.from('bbb'), 'b.txt');
  const job = await hh.waitJob((await hh.submitJob([{ capability: 'file.hash', inputs: [a] }, { capability: 'file.hash', inputs: [b] }])).id, { timeoutMs: 60000 });
  assert.equal(job.state, 'COMPLETED');
  assert.equal(job.counts.completed, 2);
  const wf = await hh.waitWorkflow((await hh.submitWorkflow([{ type: 'file.hash', params: { algorithm: 'md5' }, mode: 'perFile' }], { inputs: [a] })).id, { timeoutMs: 60000 });
  assert.equal(wf.state, 'COMPLETED');
  assert.ok(wf.outputs.length > 0);
});

t('a policy refusal is a PolicyError', async () => {
  const old = await hh.policy();
  try {
    await hh.setPolicy({ types: { 'system.identity': { enabled: false } }, allowUnlisted: true });
    await assert.rejects(hh.submitTask('system.identity'), (e) => e instanceof PolicyError && e.status === 403);
  } finally {
    await hh.setPolicy(old);
  }
});

t('waiting can time out', async () => {
  const w = await hh.submitTask('cpu.burn', { seconds: 6, threads: 1 });
  await assert.rejects(hh.waitWorkload(w.id, { timeoutMs: 500 }), WaitTimeout);
  await hh.cancelWorkload(w.id);
});

t('the event stream delivers workload events', async () => {
  const ac = new AbortController();
  const seen = [];
  const reader = (async () => {
    for await (const ev of hh.events({ signal: ac.signal })) {
      seen.push(ev);
      if (ev.type.startsWith('workload.')) break;
    }
  })();
  await sleep(800);
  await hh.submitTask('system.identity');
  await Promise.race([reader, sleep(20000)]);
  ac.abort();
  assert.ok(seen.some((e) => e.type.startsWith('workload.')), JSON.stringify(seen));
});

t('openai config, and the AI key opens /v1 only', async () => {
  const cfg = await hh.openaiConfig();
  assert.ok(cfg.baseURL.endsWith('/v1') && cfg.apiKey);
  const ai = new Client(hh.baseUrl, cfg.apiKey);
  assert.equal((await ai.request('GET', '/v1/models')).object, 'list');
  await assert.rejects(ai.nodes(), AuthenticationError);
});
