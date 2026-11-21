#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Verify the compiled plugin against real Echo + official exact-match, with simulated model inference."""
import argparse
import contextlib
import http.server
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
import hashlib

ECHO_SERVER = r'''
import os, socket, sys
from pathlib import Path
import uvicorn
from cyrene_echo.api import create_app
from cyrene_plugin_runtime import serve
from exact_match_evaluator import ExactMatchEvaluationRunner
root, fd = sys.argv[1:]
runner, ref = serve(ExactMatchEvaluationRunner(), "evaluation.runner.v1", "1", "127.0.0.1:0")
os.environ["CYRENE_EVALUATION_RUNNER_CONNECTION_REF"] = ref
app = create_app(database_path=Path(root)/"echo.db", artifact_root=Path(root)/"artifacts")
try:
    uvicorn.Server(uvicorn.Config(app, log_level="warning")).run(sockets=[socket.socket(fileno=int(fd))])
finally:
    runner.stop(grace=None).wait()
'''


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def request(url, body=None, token=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None if body is None else json.dumps(body).encode()
    with urllib.request.urlopen(urllib.request.Request(url, data, headers), timeout=10) as response:
        return json.load(response)


def wait_for(predicate, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = predicate()
            if result:
                return result
        except (OSError, ValueError):
            pass
        time.sleep(0.1)
    raise RuntimeError("local verification timed out")


@contextlib.contextmanager
def process(args, **kwargs):
    child = subprocess.Popen(args, **kwargs)
    try:
        yield child
    finally:
        child.terminate()
        try:
            child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--echo-python", type=Path, required=True)
    parser.add_argument("--plugins-root", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--pilotctl", type=Path, required=True)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    counts = {"baseline": 0, "candidate": 0}

    class SimulatedModel(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            model = data["model"]
            counts[model] += 1
            text = "A" if model == "baseline" or data["messages"][-1]["content"] == "good" else "C"
            body = json.dumps({"model": model, "choices": [{"message": {"content": text}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    with tempfile.TemporaryDirectory(prefix="pilot-echo-") as temp, contextlib.ExitStack() as stack:
        root = Path(temp)
        model = http.server.ThreadingHTTPServer(("127.0.0.1", 0), SimulatedModel)
        threading.Thread(target=model.serve_forever, daemon=True).start()
        stack.callback(model.server_close)
        stack.callback(model.shutdown)
        listener = stack.enter_context(socket.socket())
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        echo_url = f"http://127.0.0.1:{listener.getsockname()[1]}"
        log = stack.enter_context((root / "services.log").open("w+"))
        env = {**os.environ, "PYTHONPATH": str(args.plugins_root.resolve() / "plugins/evaluation/exact-match") + os.pathsep + os.environ.get("PYTHONPATH", "")}
        echo_child = stack.enter_context(process([str(args.echo_python.absolute()), "-c", ECHO_SERVER, temp, str(listener.fileno())], env=env, pass_fds=(listener.fileno(),), stdout=log, stderr=log))
        def echo_ready():
            if echo_child.poll() is not None:
                raise RuntimeError("Echo exited: " + (root / "services.log").read_text()[-6000:])
            return request(echo_url + "/healthz")
        wait_for(echo_ready)
        suite = request(echo_url + "/api/v1/evaluation-suites", {"name": "Pilot real Echo verification", "expectedField": "reference", "actualField": "actual", "threshold": 0.9})
        rows = b'{"sampleId":"one","prompt":"good","reference":"A"}\n{"sampleId":"two","prompt":"bad","reference":"A"}\n'
        private = b'{"instruction":"call 13800138000 or alice@example.com","output":"ok"}\n'
        for name, data in (("questions", rows), ("private", private)):
            (root / (name + ".jsonl")).write_bytes(data)
        token = secrets.token_urlsafe(32)
        (root / "operator.token").write_text(token)
        (root / "operator.token").chmod(0o600)
        # Reserve an ephemeral HTTP port while writing the host configuration.
        with socket.socket() as web:
            web.bind(("127.0.0.1", 0))
            port = web.getsockname()[1]
        models = {name: {"url": f"http://127.0.0.1:{model.server_port}", "model": name, "revision": "SIMULATED-v1", "maxTokens": 32, "timeoutSeconds": 5} for name in counts}
        cfg = {"httpListen": f"127.0.0.1:{port}", "adminTokenFile": str(root / "operator.token"), "automation": {"models": models, "datasets": {name: {"path": str(root / (name + ".jsonl")), "digest": digest(data)} for name, data in (("questions", rows), ("private", private))}, "dataGuard": {}, "echo": {"url": echo_url, "artifactRoot": str(root / "artifacts"), "suiteId": suite["id"], "engineBindingId": "exact-match-plugin"}}}
        (root / "config.json").write_text(json.dumps(cfg))
        storage = root / "plugin-state"
        storage.mkdir(mode=0o700)
        child = stack.enter_context(process([str(args.plugin.resolve()), "--capability", "tool.provider.v1", "--interface-version", "1", "--listen", "127.0.0.1:0", "--config", str(root / "config.json")], env={**os.environ, "CYRENE_CAPABILITY_BINDING_ID": "pilot", "CYRENE_PLUGIN_STORAGE_PATH": str(storage)}, stdout=subprocess.PIPE, stderr=log, text=True))
        wait_for(lambda: request(f"http://127.0.0.1:{port}/healthz"))
        ready = json.loads(child.stdout.readline())

        def call(tool, body):
            result = subprocess.run([str(args.pilotctl.resolve()), "--connection", ready["connection_ref"], "--binding", "pilot", "--tool", tool], input=json.dumps(body), text=True, capture_output=True, check=True)
            return json.loads(result.stdout)

        def finish(job):
            return wait_for(lambda: (j if (j := call("pilot_job_status", {"jobId": job["id"]}))["state"] not in {"QUEUED", "RUNNING"} else None))

        evaluation = finish(call("eval_replay_start", {"datasetId": "questions", "baselineId": "baseline", "candidateId": "candidate", "idempotencyKey": "actual-echo"}))
        assert evaluation["state"] == "SUCCEEDED", evaluation
        assert evaluation["summary"]["accepted"] is False, evaluation
        assert evaluation["summary"]["baseline"]["score"] == 1.0, evaluation
        assert evaluation["summary"]["candidate"]["score"] == 0.5, evaluation
        assert counts == {"baseline": 2, "candidate": 2}, counts
        guard = finish(call("data_guard_start", {"datasetId": "private", "idempotencyKey": "actual-process"}))
        assert guard["state"] == "SUCCEEDED", guard
        assert guard["summary"]["findingCount"] == 2, guard
        report = {"validation": "CPU_PROTOCOL_AND_PRODUCT_API", "echo": "REAL_PUBLIC_SOURCE", "evaluator": "OFFICIAL_EXACT_MATCH", "modelInference": "SIMULATED", "productionGPU": "NOT_RUN", "modelCalls": counts, "comparison": evaluation["summary"], "dataGuard": guard["summary"]}
        if args.report:
            args.report.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
