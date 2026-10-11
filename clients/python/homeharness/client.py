"""A small client for the Home Harness operator API (standard library only)."""

from __future__ import annotations

import hashlib
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Dict, Iterator, List, Mapping, Optional, Sequence, Union

JSON = Dict[str, Any]
FileRef = Dict[str, Any]  # {"name": ..., "sha256": ..., "size": ...}

#: States after which a workload, job or workflow will not change again.
TERMINAL_STATES = frozenset({"COMPLETED", "FAILED", "CANCELED"})


class HarnessError(Exception):
    """The manager answered with an error (or could not be reached).

    ``status`` is the HTTP status (0 when the manager could not be reached),
    ``body`` the raw response text, and ``message`` the part meant for a
    person. The manager's errors are short plain sentences saying what to do.
    """

    def __init__(self, status: int, body: str, message: Optional[str] = None):
        self.status = status
        self.body = body
        self.message = message if message is not None else _message_from(body)
        super().__init__(f"HTTP {status}: {self.message}" if status else self.message)


class AuthenticationError(HarnessError):
    """401: the token is missing or wrong."""


class PolicyError(HarnessError):
    """403: the policy does not allow this (see ``Client.policy()``)."""


class NotFoundError(HarnessError):
    """404."""


class ConflictError(HarnessError):
    """409: right now there is nowhere to run it, or the thing is in the wrong state."""


class TimeoutError_(HarnessError):
    """A wait_* call ran out of time (the work itself may still be running)."""


def _message_from(body: str) -> str:
    text = body.strip()
    try:
        data = json.loads(text)
    except ValueError:
        return text
    if isinstance(data, dict):
        err = data.get("error")
        if isinstance(err, dict):  # OpenAI style
            return str(err.get("message", text))
        if isinstance(err, str):
            return err
    return text


_ERRORS = {401: AuthenticationError, 403: PolicyError, 404: NotFoundError, 409: ConflictError}


class Client:
    """Talks to one manager.

    >>> hh = Client("http://127.0.0.1:7421", token)           # doctest: +SKIP
    >>> hh.nodes()                                            # doctest: +SKIP

    ``token`` is the operator token (the contents of the manager's
    ``harness-operator-token`` file). Keep it out of source code: read it
    from a file or an environment variable.
    """

    def __init__(self, base_url: str, token: str, timeout: float = 30.0):
        if not token:
            raise ValueError("token is required (the contents of the manager's harness-operator-token file)")
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    @classmethod
    def from_env(cls, base_url_var: str = "HARNESS_URL", token_var: str = "HARNESS_OPERATOR_TOKEN") -> "Client":
        """Build a client from HARNESS_URL (default http://127.0.0.1:7421) and HARNESS_OPERATOR_TOKEN."""
        token = os.environ.get(token_var, "")
        if not token:
            raise ValueError(f"set {token_var} to the manager's operator token")
        return cls(os.environ.get(base_url_var, "http://127.0.0.1:7421"), token)

    # ---- plumbing

    def _open(self, method: str, path: str, *, body: Optional[bytes] = None,
              content_type: Optional[str] = None, headers: Optional[Mapping[str, str]] = None,
              timeout: Optional[float] = None, auth: bool = True):
        req = urllib.request.Request(self.base_url + path, data=body, method=method)
        if auth:
            req.add_header("Authorization", "Bearer " + self.token)
        if content_type:
            req.add_header("Content-Type", content_type)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            return urllib.request.urlopen(req, timeout=self.timeout if timeout is None else timeout)
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            raise _ERRORS.get(e.code, HarnessError)(e.code, text) from None
        except urllib.error.URLError as e:
            raise HarnessError(0, str(e.reason), f"cannot reach the manager at {self.base_url}: {e.reason}") from None

    def request(self, method: str, path: str, json_body: Any = None, **kw: Any) -> Any:
        """Call any route. Returns the decoded JSON, or None for an empty body."""
        body = None if json_body is None else json.dumps(json_body).encode()
        with self._open(method, path, body=body, content_type="application/json" if body is not None else None, **kw) as r:
            raw = r.read()
        return json.loads(raw) if raw.strip() else None

    # ---- fleet

    def nodes(self) -> List[JSON]:
        """Every known node."""
        return self.request("GET", "/nodes")

    def node(self, node_id: str) -> JSON:
        return self.request("GET", "/nodes/" + _q(node_id))

    def ready_nodes(self) -> List[JSON]:
        """Nodes that are READY (connected and able to take work)."""
        return [n for n in self.nodes() if n.get("state") == "READY"]

    def set_node_meta(self, node_id: str, alias: str = "", labels: Optional[Mapping[str, str]] = None) -> JSON:
        return self.request("PUT", f"/nodes/{_q(node_id)}/meta", {"alias": alias, "labels": dict(labels or {})})

    def set_availability(self, node_id: str, mode: str = "auto", idle_minutes: int = 0, hours: str = "") -> JSON:
        body: JSON = {"mode": mode}
        if idle_minutes:
            body["idleMinutes"] = idle_minutes
        if hours:
            body["hours"] = hours
        return self.request("PUT", f"/nodes/{_q(node_id)}/availability", body)

    def revoke_node(self, node_id: str) -> JSON:
        return self.request("POST", f"/nodes/{_q(node_id)}/revoke")

    def catalog(self) -> JSON:
        """The task types, with their parameters, policy and how many nodes offer each."""
        return self.request("GET", "/catalog")

    def policy(self) -> JSON:
        return self.request("GET", "/policy")

    def set_policy(self, policy: Mapping[str, Any]) -> JSON:
        return self.request("PUT", "/policy", dict(policy))

    def audit(self, log: Optional[str] = None, limit: int = 100) -> List[JSON]:
        q = {"limit": str(limit)}
        if log:
            q["log"] = log
        return self.request("GET", "/audit?" + urllib.parse.urlencode(q))

    # ---- single tasks

    def submit_task(self, capability: str, params: Optional[Mapping[str, Any]] = None, *,
                    target: Optional[str] = None, inputs: Optional[Sequence[FileRef]] = None,
                    outputs: Optional[Sequence[str]] = None, priority: Optional[str] = None,
                    requirements: Optional[Mapping[str, Any]] = None) -> JSON:
        """Submit one typed task (see ``catalog()``). Parameter values are sent as strings."""
        body: JSON = {"capability": capability, "params": _strs(params)}
        self._fill(body, target, inputs, outputs, priority, requirements)
        return self.request("POST", "/workloads", body)

    def run_command(self, command: str, args: Sequence[str] = (), *, target: Optional[str] = None,
                    priority: Optional[str] = None) -> JSON:
        """Submit a raw command. The policy must allow raw commands (they are off by default)."""
        body: JSON = {"command": command, "args": list(args)}
        self._fill(body, target, None, None, priority, None)
        return self.request("POST", "/workloads", body)

    @staticmethod
    def _fill(body: JSON, target: Any, inputs: Any, outputs: Any, priority: Any, requirements: Any) -> None:
        if target:
            body["target"] = target
        if inputs:
            body["inputs"] = [{"name": i["name"], "sha256": i["sha256"]} for i in inputs]
        if outputs:
            body["outputs"] = list(outputs)
        if priority:
            body["priority"] = priority
        if requirements:
            body["requirements"] = dict(requirements)

    def workload(self, workload_id: str) -> JSON:
        return self.request("GET", "/workloads/" + _q(workload_id))

    def workloads(self) -> List[JSON]:
        return self.request("GET", "/workloads")

    def cancel_workload(self, workload_id: str) -> None:
        self.request("POST", f"/workloads/{_q(workload_id)}/cancel")

    def wait_workload(self, workload_id: str, timeout: float = 300.0) -> JSON:
        """Wait until the workload finishes (any terminal state) and return it. Raises on timeout."""
        return self._wait(lambda: self.workload(workload_id), timeout, f"workload {workload_id}")

    # ---- jobs, workflows

    def submit_job(self, tasks: Sequence[Mapping[str, Any]], *, reduce: Optional[Mapping[str, Any]] = None,
                   max_attempts: Optional[int] = None, name: Optional[str] = None,
                   priority: Optional[str] = None) -> JSON:
        """Submit a batch. Each task is ``{"capability": ..., "params": {...}, "inputs": [...], "outputs": [...]}``."""
        body: JSON = {"tasks": [_task(t) for t in tasks]}
        if reduce:
            body["reduce"] = _task(reduce)
        if max_attempts:
            body["maxAttempts"] = max_attempts
        if name:
            body["name"] = name
        if priority:
            body["priority"] = priority
        return self.request("POST", "/jobs", body)

    def job(self, job_id: str) -> JSON:
        return self.request("GET", "/jobs/" + _q(job_id))

    def jobs(self) -> List[JSON]:
        return self.request("GET", "/jobs")

    def cancel_job(self, job_id: str) -> JSON:
        return self.request("POST", f"/jobs/{_q(job_id)}/cancel")

    def wait_job(self, job_id: str, timeout: float = 3600.0) -> JSON:
        return self._wait(lambda: self.job(job_id), timeout, f"job {job_id}")

    def submit_workflow(self, stages: Sequence[Mapping[str, Any]], *, inputs: Optional[Sequence[FileRef]] = None,
                        name: Optional[str] = None, max_attempts: Optional[int] = None) -> JSON:
        """Submit steps that run in order. A stage is ``{"type": ..., "params": {...}, "mode": "perFile"}``."""
        body: JSON = {"stages": [_stage(s) for s in stages]}
        if inputs:
            body["inputs"] = [{"name": i["name"], "sha256": i["sha256"]} for i in inputs]
        if name:
            body["name"] = name
        if max_attempts:
            body["maxAttempts"] = max_attempts
        return self.request("POST", "/workflows", body)

    def workflow(self, workflow_id: str) -> JSON:
        return self.request("GET", "/workflows/" + _q(workflow_id))

    def cancel_workflow(self, workflow_id: str) -> JSON:
        return self.request("POST", f"/workflows/{_q(workflow_id)}/cancel")

    def wait_workflow(self, workflow_id: str, timeout: float = 3600.0) -> JSON:
        return self._wait(lambda: self.workflow(workflow_id), timeout, f"workflow {workflow_id}")

    # ---- AI plans

    def plan(self, request: str, model: str, files: Optional[Sequence[FileRef]] = None) -> JSON:
        """Ask a local model to propose a job. Nothing runs until ``approve_plan``."""
        body: JSON = {"request": request, "model": model, "files": [{"name": f["name"], "sha256": f["sha256"]} for f in files or []]}
        return self.request("POST", "/plans", body)

    def plans(self) -> JSON:
        """``{"plans": [...], "models": [...]}``: the plans and the models that can plan now."""
        return self.request("GET", "/plans")

    def get_plan(self, plan_id: str) -> JSON:
        return self.request("GET", "/plans/" + _q(plan_id))

    def approve_plan(self, plan_id: str) -> JSON:
        return self.request("POST", f"/plans/{_q(plan_id)}/approve")

    def reject_plan(self, plan_id: str) -> JSON:
        return self.request("POST", f"/plans/{_q(plan_id)}/reject")

    def wait_plan(self, plan_id: str, timeout: float = 600.0) -> JSON:
        """Wait while the model is planning; returns the plan (proposed, refused or failed)."""
        return self._wait(lambda: self.get_plan(plan_id), timeout, f"plan {plan_id}",
                          done=lambda p: p.get("state") != "planning")

    # ---- files

    def upload_artifact(self, source: Union[str, os.PathLike, bytes], name: Optional[str] = None) -> FileRef:
        """Store a file (a path or bytes). Returns ``{"name", "sha256", "size"}`` ready to pass as an input."""
        if isinstance(source, (bytes, bytearray)):
            data = bytes(source)
            if not name:
                raise ValueError("name is required when uploading bytes")
        else:
            with open(source, "rb") as f:
                data = f.read()
            name = name or os.path.basename(os.fspath(source))
        digest = hashlib.sha256(data).hexdigest()
        with self._open("POST", "/artifacts", body=data, content_type="application/octet-stream",
                        headers={"X-Artifact-SHA256": digest}) as r:
            info = json.loads(r.read())
        return {"name": name, "sha256": info["sha256"], "size": info["size"]}

    def artifacts(self) -> JSON:
        return self.request("GET", "/artifacts")

    def download_artifact(self, sha256: str, dest: Union[str, os.PathLike, None] = None) -> Union[bytes, str]:
        """Fetch a stored file. With ``dest`` it is written there (and the path returned); else the bytes are returned.
        The download is checked against its SHA-256."""
        with self._open("GET", "/artifacts/" + _q(sha256)) as r:
            data = r.read()
        if hashlib.sha256(data).hexdigest() != sha256:
            raise HarnessError(0, "", "the downloaded file does not match its SHA-256; not saved")
        if dest is None:
            return data
        os.makedirs(os.path.dirname(os.path.abspath(dest)), exist_ok=True)
        with open(dest, "wb") as f:
            f.write(data)
        return os.fspath(dest)

    def download_outputs(self, result: Mapping[str, Any], dest_dir: Union[str, os.PathLike]) -> List[str]:
        """Download every output file of a finished workload, job or workflow into ``dest_dir``.

        Output names such as ``out/result.txt`` keep their folders; names that would escape ``dest_dir`` are refused.
        """
        refs = result.get("outputs") or []
        if refs and isinstance(refs[0], str):  # a workload's declared names; the delivered files are outputFiles
            refs = result.get("outputFiles") or []
        root = os.path.abspath(dest_dir)
        written = []
        for ref in refs:
            target = os.path.abspath(os.path.join(root, *ref["name"].split("/")))
            if not target.startswith(root + os.sep):
                raise HarnessError(0, "", f"refusing output name {ref['name']!r}: it leaves the destination folder")
            written.append(str(self.download_artifact(ref["sha256"], target)))
        return written

    # ---- events

    def events(self, timeout: Optional[float] = None) -> Iterator[JSON]:
        """Yield harness events as they happen (Server-Sent Events). Runs until closed.

        There is no replay: events before you connect are not delivered, and a
        slow reader may miss some. Treat an event as a hint to re-read state.
        ``timeout`` is how long to wait for any data (None: forever). Close the
        generator (or break out of the loop) to disconnect.
        """
        resp = self._open("GET", "/events", timeout=timeout, headers={"Accept": "text/event-stream"})
        try:
            data: List[str] = []
            for raw in resp:
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                if line == "":
                    if data:
                        try:
                            yield json.loads("\n".join(data))
                        except ValueError:
                            pass
                        data = []
                elif line.startswith("data:"):
                    data.append(line[5:].lstrip(" "))
        finally:
            resp.close()

    # ---- AI (OpenAI-compatible)

    def ai_info(self) -> JSON:
        """``{"baseUrl", "apiKey", "models"}`` for the OpenAI-compatible API. The apiKey is a secret."""
        return self.request("GET", "/ai-info")

    def models(self) -> List[str]:
        """The model names a chat can ask for now."""
        return list(self.ai_info().get("models", []))

    def openai_config(self) -> Dict[str, str]:
        """``{"base_url", "api_key"}``: pass to any OpenAI-compatible library."""
        info = self.ai_info()
        return {"base_url": info["baseUrl"], "api_key": info["apiKey"]}

    def openai_client(self, **kwargs: Any):
        """An ``openai.OpenAI`` client pointed at this manager's /v1. Needs ``pip install openai``."""
        try:
            import openai  # type: ignore
        except ImportError as e:  # pragma: no cover
            raise ImportError("install the openai package (pip install openai), or use openai_config()") from e
        return openai.OpenAI(**{**self.openai_config(), **kwargs})

    # ---- waiting

    def _wait(self, fetch: Callable[[], JSON], timeout: float, what: str,
              done: Optional[Callable[[JSON], bool]] = None) -> JSON:
        done = done or (lambda o: o.get("state") in TERMINAL_STATES)
        deadline = time.monotonic() + timeout
        delay = 0.2
        while True:
            obj = fetch()
            if done(obj):
                return obj
            if time.monotonic() >= deadline:
                raise TimeoutError_(0, "", f"{what} did not finish within {timeout:g} seconds (it may still be running)")
            time.sleep(min(delay, max(0.0, deadline - time.monotonic())))
            delay = min(delay * 1.5, 2.0)


def _q(part: str) -> str:
    return urllib.parse.quote(str(part), safe="")


def _strs(params: Optional[Mapping[str, Any]]) -> Dict[str, str]:
    """The manager takes every parameter as a string; booleans become true/false."""
    out = {}
    for k, v in (params or {}).items():
        out[k] = ("true" if v else "false") if isinstance(v, bool) else str(v)
    return out


def _task(t: Mapping[str, Any]) -> JSON:
    out = {k: v for k, v in t.items() if k != "params"}
    if "params" in t:
        out["params"] = _strs(t["params"])
    if "capability" not in out and "command" not in out:
        raise ValueError("a task needs a capability (or a command)")
    return out


def _stage(s: Mapping[str, Any]) -> JSON:
    out = {k: v for k, v in s.items() if k not in ("params", "combine")}
    if "params" in s:
        out["params"] = _strs(s["params"])
    if s.get("combine"):
        c = dict(s["combine"])
        if "params" in c:
            c["params"] = _strs(c["params"])
        out["combine"] = c
    if "type" not in out:
        raise ValueError("a workflow stage needs a type")
    return out
