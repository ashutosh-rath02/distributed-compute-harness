// A small client for the Home Harness operator API. Node 20+, ES modules,
// global fetch, no dependencies. The API is described in docs/openapi.yaml.

import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { basename, dirname, resolve, sep, join } from 'node:path';

/**
 * @typedef {{name: string, sha256: string, size?: number}} FileRef
 * @typedef {Record<string, any>} Json
 * @typedef {{capability: string, params?: Record<string, any>, inputs?: FileRef[], outputs?: string[], target?: string, command?: string, args?: string[]}} TaskSpec
 * @typedef {{type: string, params?: Record<string, any>, mode?: 'once'|'perFile'|'parts', parts?: number, name?: string, combine?: {type: string, params?: Record<string, any>}}} StageSpec
 */

/** States after which a workload, job or workflow will not change again. */
export const TERMINAL_STATES = new Set(['COMPLETED', 'FAILED', 'CANCELED']);

/**
 * The manager answered with an error, or could not be reached.
 * `status` is the HTTP status (0: not reached), `body` the raw text and
 * `message` the part meant for a person.
 */
export class HarnessError extends Error {
  /** @param {number} status @param {string} body @param {string} [message] */
  constructor(status, body, message) {
    const text = message ?? messageFrom(body);
    super(status ? `HTTP ${status}: ${text}` : text);
    this.name = 'HarnessError';
    this.status = status;
    this.body = body;
    this.detail = text;
  }
}
export class AuthenticationError extends HarnessError { constructor(...a) { super(...a); this.name = 'AuthenticationError'; } }
export class PolicyError extends HarnessError { constructor(...a) { super(...a); this.name = 'PolicyError'; } }
export class NotFoundError extends HarnessError { constructor(...a) { super(...a); this.name = 'NotFoundError'; } }
export class ConflictError extends HarnessError { constructor(...a) { super(...a); this.name = 'ConflictError'; } }
export class WaitTimeout extends HarnessError { constructor(...a) { super(...a); this.name = 'WaitTimeout'; } }

const ERRORS = { 401: AuthenticationError, 403: PolicyError, 404: NotFoundError, 409: ConflictError };

function messageFrom(body) {
  const text = String(body).trim();
  try {
    const data = JSON.parse(text);
    if (data && typeof data === 'object') {
      if (data.error && typeof data.error === 'object') return String(data.error.message ?? text);
      if (typeof data.error === 'string') return data.error;
    }
  } catch { /* plain text */ }
  return text;
}

const q = (s) => encodeURIComponent(String(s));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** The manager takes every parameter as a string; booleans become true/false. */
function strs(params) {
  const out = {};
  for (const [k, v] of Object.entries(params ?? {})) out[k] = typeof v === 'boolean' ? String(v) : String(v);
  return out;
}
const refs = (list) => (list ?? []).map((i) => ({ name: i.name, sha256: i.sha256 }));

function taskBody(t) {
  if (!t.capability && !t.command) throw new TypeError('a task needs a capability (or a command)');
  const out = { ...t };
  if (t.params) out.params = strs(t.params);
  if (t.inputs) out.inputs = refs(t.inputs);
  return out;
}

function stageBody(s) {
  if (!s.type) throw new TypeError('a workflow stage needs a type');
  const out = { ...s };
  if (s.params) out.params = strs(s.params);
  if (s.combine) out.combine = { ...s.combine, ...(s.combine.params ? { params: strs(s.combine.params) } : {}) };
  return out;
}

export class Client {
  /**
   * @param {string} baseUrl e.g. http://127.0.0.1:7421
   * @param {string} token the operator token (the manager's harness-operator-token file). Keep it out of source code.
   * @param {{timeoutMs?: number}} [opts]
   */
  constructor(baseUrl, token, opts = {}) {
    if (!token) throw new TypeError("token is required (the contents of the manager's harness-operator-token file)");
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.token = token;
    this.timeoutMs = opts.timeoutMs ?? 30000;
  }

  /** Build from HARNESS_URL (default http://127.0.0.1:7421) and HARNESS_OPERATOR_TOKEN. */
  static fromEnv(env = process.env) {
    if (!env.HARNESS_OPERATOR_TOKEN) throw new Error("set HARNESS_OPERATOR_TOKEN to the manager's operator token");
    return new Client(env.HARNESS_URL || 'http://127.0.0.1:7421', env.HARNESS_OPERATOR_TOKEN);
  }

  /** @returns {Promise<Response>} */
  async #fetch(method, path, { body, contentType, headers, timeoutMs, signal } = {}) {
    const h = { Authorization: `Bearer ${this.token}`, ...(headers ?? {}) };
    if (contentType) h['Content-Type'] = contentType;
    const signals = [];
    if ((timeoutMs ?? this.timeoutMs) > 0) signals.push(AbortSignal.timeout(timeoutMs ?? this.timeoutMs));
    if (signal) signals.push(signal);
    let res;
    try {
      res = await fetch(this.baseUrl + path, { method, headers: h, body, signal: signals.length ? AbortSignal.any(signals) : undefined });
    } catch (e) {
      if (signal?.aborted) throw e;
      throw new HarnessError(0, '', `cannot reach the manager at ${this.baseUrl}: ${e.cause?.code ?? e.message}`);
    }
    if (!res.ok) {
      const text = await res.text();
      throw new (ERRORS[res.status] ?? HarnessError)(res.status, text);
    }
    return res;
  }

  /** Call any route. Returns decoded JSON, or null for an empty body. */
  async request(method, path, json, opts = {}) {
    const res = await this.#fetch(method, path, json === undefined ? opts : { ...opts, body: JSON.stringify(json), contentType: 'application/json' });
    const text = await res.text();
    return text.trim() ? JSON.parse(text) : null;
  }

  // ---- fleet
  nodes() { return this.request('GET', '/nodes'); }
  node(id) { return this.request('GET', `/nodes/${q(id)}`); }
  async readyNodes() { return (await this.nodes()).filter((n) => n.state === 'READY'); }
  setNodeMeta(id, alias = '', labels = {}) { return this.request('PUT', `/nodes/${q(id)}/meta`, { alias, labels }); }
  setAvailability(id, { mode = 'auto', idleMinutes, hours } = {}) {
    return this.request('PUT', `/nodes/${q(id)}/availability`, { mode, ...(idleMinutes ? { idleMinutes } : {}), ...(hours ? { hours } : {}) });
  }
  revokeNode(id) { return this.request('POST', `/nodes/${q(id)}/revoke`); }
  catalog() { return this.request('GET', '/catalog'); }
  policy() { return this.request('GET', '/policy'); }
  setPolicy(policy) { return this.request('PUT', '/policy', policy); }
  audit({ log, limit = 100 } = {}) { return this.request('GET', `/audit?limit=${limit}${log ? `&log=${q(log)}` : ''}`); }

  // ---- single tasks
  /**
   * Submit one typed task (see catalog()). Parameter values are sent as strings.
   * @param {string} capability
   * @param {Record<string, any>} [params]
   * @param {{target?: string, inputs?: FileRef[], outputs?: string[], priority?: string, requirements?: Json}} [opts]
   */
  submitTask(capability, params = {}, opts = {}) {
    return this.request('POST', '/workloads', this.#workloadBody({ capability, params: strs(params) }, opts));
  }
  /** Submit a raw command. The policy must allow raw commands (off by default). */
  runCommand(command, args = [], opts = {}) {
    return this.request('POST', '/workloads', this.#workloadBody({ command, args }, opts));
  }
  #workloadBody(body, { target, inputs, outputs, priority, requirements } = {}) {
    if (target) body.target = target;
    if (inputs?.length) body.inputs = refs(inputs);
    if (outputs?.length) body.outputs = outputs;
    if (priority) body.priority = priority;
    if (requirements) body.requirements = requirements;
    return body;
  }
  workload(id) { return this.request('GET', `/workloads/${q(id)}`); }
  workloads() { return this.request('GET', '/workloads'); }
  async cancelWorkload(id) { await this.request('POST', `/workloads/${q(id)}/cancel`); }
  /** Wait until the workload finishes and return it. Rejects with WaitTimeout. */
  waitWorkload(id, { timeoutMs = 300000 } = {}) { return this.#wait(() => this.workload(id), timeoutMs, `workload ${id}`); }

  // ---- jobs and workflows
  /**
   * @param {TaskSpec[]} tasks
   * @param {{reduce?: TaskSpec, maxAttempts?: number, name?: string, priority?: string}} [opts]
   */
  submitJob(tasks, { reduce, maxAttempts, name, priority } = {}) {
    return this.request('POST', '/jobs', {
      tasks: tasks.map(taskBody),
      ...(reduce ? { reduce: taskBody(reduce) } : {}), ...(maxAttempts ? { maxAttempts } : {}),
      ...(name ? { name } : {}), ...(priority ? { priority } : {}),
    });
  }
  job(id) { return this.request('GET', `/jobs/${q(id)}`); }
  jobs() { return this.request('GET', '/jobs'); }
  cancelJob(id) { return this.request('POST', `/jobs/${q(id)}/cancel`); }
  waitJob(id, { timeoutMs = 3600000 } = {}) { return this.#wait(() => this.job(id), timeoutMs, `job ${id}`); }

  /**
   * Steps that run in order, each on the step before's results.
   * @param {StageSpec[]} stages
   * @param {{inputs?: FileRef[], name?: string, maxAttempts?: number}} [opts]
   */
  submitWorkflow(stages, { inputs, name, maxAttempts } = {}) {
    return this.request('POST', '/workflows', {
      stages: stages.map(stageBody),
      ...(inputs?.length ? { inputs: refs(inputs) } : {}), ...(name ? { name } : {}), ...(maxAttempts ? { maxAttempts } : {}),
    });
  }
  workflow(id) { return this.request('GET', `/workflows/${q(id)}`); }
  cancelWorkflow(id) { return this.request('POST', `/workflows/${q(id)}/cancel`); }
  waitWorkflow(id, { timeoutMs = 3600000 } = {}) { return this.#wait(() => this.workflow(id), timeoutMs, `workflow ${id}`); }

  // ---- AI plans
  /** Ask a local model to propose a job. Nothing runs until approvePlan(). */
  plan(request, model, files = []) { return this.request('POST', '/plans', { request, model, files: refs(files) }); }
  /** `{plans, models}`: the plans and the models that can plan now. */
  plans() { return this.request('GET', '/plans'); }
  getPlan(id) { return this.request('GET', `/plans/${q(id)}`); }
  approvePlan(id) { return this.request('POST', `/plans/${q(id)}/approve`); }
  rejectPlan(id) { return this.request('POST', `/plans/${q(id)}/reject`); }
  /** Wait while the model is planning; resolves with the plan (proposed, refused or failed). */
  waitPlan(id, { timeoutMs = 600000 } = {}) {
    return this.#wait(() => this.getPlan(id), timeoutMs, `plan ${id}`, (p) => p.state !== 'planning');
  }

  // ---- files
  /**
   * Store a file (a path, or bytes with a name). Returns {name, sha256, size}, ready to pass as an input.
   * @param {string | Uint8Array} source
   * @param {string} [name]
   * @returns {Promise<FileRef>}
   */
  async uploadArtifact(source, name) {
    let data;
    if (typeof source === 'string') { data = await readFile(source); name ??= basename(source); }
    else { data = Buffer.from(source); if (!name) throw new TypeError('name is required when uploading bytes'); }
    const sha256 = createHash('sha256').update(data).digest('hex');
    const res = await this.#fetch('POST', '/artifacts', { body: data, contentType: 'application/octet-stream', headers: { 'X-Artifact-SHA256': sha256 } });
    const info = await res.json();
    return { name, sha256: info.sha256, size: info.size };
  }
  artifacts() { return this.request('GET', '/artifacts'); }
  /**
   * Fetch a stored file, checked against its SHA-256. With `dest` it is written there and the path returned; else a Buffer.
   * @param {string} sha256
   * @param {string} [dest]
   */
  async downloadArtifact(sha256, dest) {
    const res = await this.#fetch('GET', `/artifacts/${q(sha256)}`);
    const data = Buffer.from(await res.arrayBuffer());
    if (createHash('sha256').update(data).digest('hex') !== sha256) throw new HarnessError(0, '', 'the downloaded file does not match its SHA-256; not saved');
    if (!dest) return data;
    await mkdir(dirname(resolve(dest)), { recursive: true });
    await writeFile(dest, data);
    return dest;
  }
  /** Download every output of a finished workload, job or workflow into destDir. Names that leave destDir are refused. */
  async downloadOutputs(result, destDir) {
    let list = result.outputs ?? [];
    if (list.length && typeof list[0] === 'string') list = result.outputFiles ?? [];
    const root = resolve(destDir);
    const written = [];
    for (const ref of list) {
      const target = resolve(join(root, ...ref.name.split('/')));
      if (!target.startsWith(root + sep)) throw new HarnessError(0, '', `refusing output name ${JSON.stringify(ref.name)}: it leaves the destination folder`);
      written.push(await this.downloadArtifact(ref.sha256, target));
    }
    return written;
  }

  // ---- events
  /**
   * Harness events as they happen (Server-Sent Events), as an async iterator. There is no replay, and a slow reader may
   * miss events: treat one as a hint to re-read state. Break out of the loop, or abort `signal`, to disconnect.
   * @param {{signal?: AbortSignal}} [opts]
   * @returns {AsyncGenerator<Json>}
   */
  async *events({ signal } = {}) {
    const res = await this.#fetch('GET', '/events', { headers: { Accept: 'text/event-stream' }, timeoutMs: 0, signal });
    const decoder = new TextDecoder();
    let buf = '';
    let data = [];
    try {
      for await (const chunk of res.body) {
        buf += decoder.decode(chunk, { stream: true });
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, i).replace(/\r$/, '');
          buf = buf.slice(i + 1);
          if (line === '') {
            if (data.length) { try { yield JSON.parse(data.join('\n')); } catch (e) { if (e instanceof SyntaxError) { /* skip */ } else throw e; } }
            data = [];
          } else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''));
        }
      }
    } catch (e) {
      if (!signal?.aborted) throw e;
    } finally {
      res.body?.cancel().catch(() => {});
    }
  }

  // ---- AI (OpenAI-compatible)
  /** `{baseUrl, apiKey, models}` for the OpenAI-compatible API. The apiKey is a secret. */
  aiInfo() { return this.request('GET', '/ai-info'); }
  async models() { return (await this.aiInfo()).models ?? []; }
  /** `{baseURL, apiKey}`: the option names the `openai` npm package uses, e.g. `new OpenAI(await hh.openaiConfig())`. */
  async openaiConfig() { const i = await this.aiInfo(); return { baseURL: i.baseUrl, apiKey: i.apiKey }; }

  // ---- waiting
  async #wait(fetchFn, timeoutMs, what, done = (o) => TERMINAL_STATES.has(o.state)) {
    const deadline = Date.now() + timeoutMs;
    let delay = 200;
    for (;;) {
      const obj = await fetchFn();
      if (done(obj)) return obj;
      if (Date.now() >= deadline) throw new WaitTimeout(0, '', `${what} did not finish within ${timeoutMs / 1000} seconds (it may still be running)`);
      await sleep(Math.min(delay, Math.max(0, deadline - Date.now())));
      delay = Math.min(delay * 1.5, 2000);
    }
  }
}
