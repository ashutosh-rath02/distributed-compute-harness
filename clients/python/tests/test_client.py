"""Tests against a throwaway manager and agent.

Set HH_MANAGER_BIN and HH_AGENT_BIN to built binaries (go build ./cmd/manager
./cmd/agent). HH_TEST_PORT picks the first of three loopback ports (default
18160). Without the binaries, the tests that need them are skipped. All state
goes in a temporary folder; nothing outside it is touched.

    python -m unittest discover -s tests
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from homeharness import AuthenticationError, Client, HarnessError, NotFoundError, WaitTimeout  # noqa: E402
from homeharness.client import _strs, _task, _stage  # noqa: E402

MANAGER = os.environ.get("HH_MANAGER_BIN", "")
AGENT = os.environ.get("HH_AGENT_BIN", "")
PORT = int(os.environ.get("HH_TEST_PORT", "18160"))


class UnitTests(unittest.TestCase):
    def test_token_is_required(self):
        with self.assertRaises(ValueError):
            Client("http://127.0.0.1:1", "")

    def test_param_values_become_strings(self):
        self.assertEqual(_strs({"a": 1, "b": True, "c": False, "d": "x", "e": 1.5}),
                         {"a": "1", "b": "true", "c": "false", "d": "x", "e": "1.5"})

    def test_task_and_stage_validation(self):
        with self.assertRaises(ValueError):
            _task({"params": {}})
        with self.assertRaises(ValueError):
            _stage({"params": {}})
        self.assertEqual(_stage({"type": "t", "params": {"n": 2}, "combine": {"type": "c", "params": {"k": 3}}}),
                         {"type": "t", "params": {"n": "2"}, "combine": {"type": "c", "params": {"k": "3"}}})

    def test_unreachable_manager_is_an_error_with_status_zero(self):
        c = Client("http://127.0.0.1:%d" % (PORT + 9), "t" * 40, timeout=2)
        with self.assertRaises(HarnessError) as cm:
            c.nodes()
        self.assertEqual(cm.exception.status, 0)
        self.assertIn("cannot reach the manager", str(cm.exception))


@unittest.skipUnless(os.path.exists(MANAGER) and os.path.exists(AGENT), "set HH_MANAGER_BIN and HH_AGENT_BIN")
class LiveTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp(prefix="hh-pyclient-")
        cls.api = "127.0.0.1:%d" % PORT
        cls.agent_addr = "127.0.0.1:%d" % (PORT + 1)
        cls.procs = []
        cls.logs = []
        token_file = os.path.join(cls.dir, "operator-token")
        cls._start([MANAGER, "-disable-discovery", "-keep-awake=false",
                    "-addr", cls.agent_addr, "-api-addr", cls.api, "-pairing-token", "test-pairing-secret-0123456789",
                    "-db", os.path.join(cls.dir, "m.db"), "-tls-dir", os.path.join(cls.dir, "tls"),
                    "-operator-token-file", token_file, "-artifact-dir", os.path.join(cls.dir, "art"),
                    "-ai-key-file", os.path.join(cls.dir, "ai-key"),
                    "-remote-access-file", os.path.join(cls.dir, "remote"), "-allow-raw-commands"], "manager")
        cls._wait_for(lambda: os.path.exists(token_file) and urllib.request.urlopen("http://%s/" % cls.api, timeout=1))
        with open(token_file) as f:
            cls.token = f.read().strip()
        cls.hh = Client("http://" + cls.api, cls.token)
        fingerprint = cls.hh.request("GET", "/join-info")["fingerprint"]
        cls._start([AGENT, "-manager-fingerprint", fingerprint, "-manager-addr", cls.agent_addr, "-pairing-token", "test-pairing-secret-0123456789",
                    "-identity-dir", os.path.join(cls.dir, "id"), "-work-dir", os.path.join(cls.dir, "work"),
                    "-name", "py-test-agent", "-keep-awake=false"], "agent")
        cls._wait_for(lambda: cls.hh.ready_nodes())

    @classmethod
    def _start(cls, argv, label):
        log = open(os.path.join(cls.dir, label + ".log"), "wb")
        cls.logs.append(log)
        cls.procs.append(subprocess.Popen(argv, cwd=cls.dir, stdout=log, stderr=log))

    @classmethod
    def _wait_for(cls, fn, seconds=40):
        end = time.time() + seconds
        while time.time() < end:
            try:
                if fn():
                    return
            except Exception:
                pass
            time.sleep(0.3)
        raise RuntimeError("timed out waiting for the throwaway manager/agent; logs in " + cls.dir)

    @classmethod
    def tearDownClass(cls):
        for p in cls.procs:  # only the processes this test started
            p.terminate()
        for p in cls.procs:
            try:
                p.wait(10)
            except subprocess.TimeoutExpired:
                p.kill()
        for log in cls.logs:
            log.close()
        shutil.rmtree(cls.dir, ignore_errors=True)

    def test_wrong_token_is_authentication_error(self):
        bad = Client("http://" + self.api, "x" * 64)
        with self.assertRaises(AuthenticationError) as cm:
            bad.nodes()
        self.assertEqual(cm.exception.status, 401)
        self.assertIn("operator authentication required", cm.exception.message)

    def test_not_found_keeps_status_and_body(self):
        with self.assertRaises(NotFoundError) as cm:
            self.hh.workload("no-such-id")
        self.assertEqual(cm.exception.status, 404)
        self.assertIn("not found", cm.exception.body)

    def test_nodes_and_catalog(self):
        nodes = self.hh.ready_nodes()
        self.assertEqual(nodes[0]["name"], "py-test-agent")
        types = {t["name"] for t in self.hh.catalog()["types"]}
        self.assertIn("file.hash", types)
        self.assertIn("nodes", self.hh.catalog()["types"][0])

    def test_task_with_upload_wait_and_download(self):
        ref = self.hh.upload_artifact(b"hello harness\n", name="hello.txt")
        self.assertEqual(len(ref["sha256"]), 64)
        w = self.hh.submit_task("file.hash", {"algorithm": "sha256"}, inputs=[ref])
        done = self.hh.wait_workload(w["id"], timeout=60)
        self.assertEqual(done["state"], "COMPLETED", done)
        out = os.path.join(self.dir, "dl")
        paths = self.hh.download_outputs(done, out)
        self.assertTrue(paths)
        with open(paths[0]) as f:
            self.assertIn("hello.txt", f.read())

    def test_job_with_two_tasks(self):
        a = self.hh.upload_artifact(b"aaa", name="a.txt")
        b = self.hh.upload_artifact(b"bbb", name="b.txt")
        job = self.hh.submit_job([{"capability": "file.hash", "inputs": [a]}, {"capability": "file.hash", "inputs": [b]}], name="py")
        done = self.hh.wait_job(job["id"], timeout=60)
        self.assertEqual(done["state"], "COMPLETED", done)
        self.assertEqual(done["counts"]["completed"], 2)

    def test_workflow(self):
        a = self.hh.upload_artifact(b"wf", name="w.txt")
        wf = self.hh.submit_workflow([{"type": "file.hash", "params": {"algorithm": "md5"}, "mode": "perFile"}], inputs=[a])
        done = self.hh.wait_workflow(wf["id"], timeout=60)
        self.assertEqual(done["state"], "COMPLETED", done)
        self.assertTrue(done["outputs"])

    def test_policy_refusal_is_a_policy_error(self):
        from homeharness import PolicyError
        pol = self.hh.policy()
        try:
            self.hh.set_policy({"types": {"system.identity": {"enabled": False}}, "allowUnlisted": True})
            with self.assertRaises(PolicyError) as cm:
                self.hh.submit_task("system.identity")
            self.assertEqual(cm.exception.status, 403)
        finally:
            self.hh.set_policy(pol)

    def test_wait_timeout(self):
        w = self.hh.submit_task("cpu.burn", {"seconds": 6, "threads": 1})
        with self.assertRaises(WaitTimeout):
            self.hh.wait_workload(w["id"], timeout=0.5)
        self.hh.cancel_workload(w["id"])

    def test_events_stream(self):
        got = []

        def listen():
            for ev in self.hh.events(timeout=30):
                got.append(ev)
                if ev.get("type", "").startswith("workload."):
                    return

        t = threading.Thread(target=listen, daemon=True)
        t.start()
        time.sleep(0.8)
        self.hh.submit_task("system.identity")
        t.join(20)
        self.assertTrue(any(e["type"].startswith("workload.") for e in got), got)

    def test_ai_config_and_plan_models(self):
        cfg = self.hh.openai_config()
        self.assertTrue(cfg["base_url"].endswith("/v1"))
        self.assertTrue(cfg["api_key"])
        self.assertIn("models", self.hh.plans())
        # the AI key opens /v1 only
        ai = Client("http://" + self.api, cfg["api_key"])
        self.assertEqual(ai.request("GET", "/v1/models")["object"], "list")
        with self.assertRaises(AuthenticationError):
            ai.nodes()


if __name__ == "__main__":
    unittest.main()
