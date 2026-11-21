#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Run the CPU release workflow against Platform, Navigator and four real Product APIs."""

import argparse
import contextlib
import json
import os
import secrets
import select
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent
BINDING = "release-pilot"
WORK = "/api/v1/workspaces/cpu-pilot/work/tool-providers"
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def exchange(origin, path, token=None, data=None, expected=200, key=None):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if data is not None:
        headers["Content-Type"] = "application/json"
    if key:
        headers["Idempotency-Key"] = key
    request = urllib.request.Request(
        origin + path,
        headers=headers,
        data=None if data is None else json.dumps(data).encode(),
    )
    try:
        response = HTTP.open(request, timeout=12)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        body = response.read()
        if response.status != expected:
            raise RuntimeError(
                f"{request.get_method()} {path}: HTTP {response.status}: {body[:1000]!r}"
            )
        return json.loads(body)


def ready(origin, process):
    def check():
        if process.poll() is not None:
            raise RuntimeError(f"service stopped with status {process.returncode}")
        try:
            return exchange(origin, "/healthz")
        except (urllib.error.URLError, ConnectionError):
            return None

    wait_for(check, 20, f"service at {origin}")


def wait_for(probe, seconds, label):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        result = probe()
        if result:
            return result
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {label}")


def listen():
    value = socket.socket()
    value.bind(("127.0.0.1", 0))
    value.listen(64)
    return value


def origin(sock):
    return f"http://127.0.0.1:{sock.getsockname()[1]}"


def private_file(root, name, value):
    path = root / name
    with os.fdopen(
        os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w"
    ) as file:
        file.write(value)
    return path


def child_line(process):
    if not select.select([process.stdout], [], [], 30)[0]:
        raise RuntimeError("local launcher did not publish readiness")
    raw = process.stdout.readline()
    if not raw:
        raise RuntimeError(f"local launcher exited before readiness: {process.poll()}")
    return json.loads(raw)


@contextlib.contextmanager
def process(*args, **kwargs):
    with subprocess.Popen([str(a) for a in args], **kwargs) as child:
        try:
            yield child
        finally:
            if child.poll() is None and child.stdin:
                child.stdin.close()
                try:
                    child.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    pass
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    child.kill()


def install_baseline(settings, tokens, model):
    deployed = exchange(
        settings["reactor"],
        "/api/v1/deployments",
        tokens["reactor"],
        {
            "name": "CPU baseline",
            "modelArtifact": model,
            "servingBindingId": "default",
        },
        expected=201,
        key="cpu-baseline-deploy",
    )

    def lookup():
        value = exchange(
            settings["reactor"],
            f"/api/v1/deployments/{deployed['id']}",
            tokens["reactor"],
        )
        return value if value["observedState"] == "READY" else None

    deployed = wait_for(lookup, 20, "baseline deployment")
    endpoint = exchange(
        settings["reactor"],
        f"/api/v1/endpoints/{deployed['endpointId']}",
        tokens["reactor"],
    )
    draft = exchange(
        settings["reactor"],
        f"/api/v1/endpoints/{endpoint['id']}/actions/send-to-exchange",
        tokens["reactor"],
        {
            "resourceVersion": endpoint["resourceVersion"],
            "receiverId": "pilot",
            "gatewayEndpointId": settings["gateway"],
            "targetBindingId": "baseline",
            "modelPattern": "cpu-baseline",
        },
        expected=201,
        key="cpu-baseline-route",
    )["route"]
    return exchange(
        settings["exchange"],
        f"/api/v1/gateway-route-drafts/{draft['id']}/actions/confirm",
        tokens["exchange"],
        {
            "resourceVersion": draft["resourceVersion"],
        },
    )


def run(args, root, stack):
    tokens = {
        name: secrets.token_urlsafe(32)
        for name in (
            "reactor",
            "exchange",
            "inference",
            "admin",
            "pilot",
            "navigator",
            "viewer",
        )
    }
    for name in ("reactor", "exchange", "inference", "admin", "pilot"):
        private_file(root, f"{name}.token", tokens[name])
    names = ("catalyst", "echo", "exchange", "reactor")
    sockets = {name: stack.enter_context(listen()) for name in names}
    settings = {name: origin(listener) for name, listener in sockets.items()}
    settings["gateway"] = str(uuid.uuid4())
    (root / "origins.json").write_text(json.dumps(settings))
    for name in names:
        listener = sockets[name]
        child = stack.enter_context(
            process(
                getattr(args, name + "_python"),
                ROOT / "demo_services.py",
                name,
                root,
                listener.fileno(),
                pass_fds=(listener.fileno(),),
            )
        )
        listener.close()
        ready(settings[name], child)

    inputs = json.loads((root / "inputs.json").read_text())
    suite = exchange(
        settings["echo"],
        "/api/v1/evaluation-suites",
        data={
            "name": "CPU release benchmark",
            "threshold": 0.90,
            "expectedField": "reference",
            "actualField": "actual",
        },
        expected=201,
    )
    for item in inputs.values():
        item["inputId"] = exchange(
            settings["echo"],
            "/api/v1/evaluation-inputs",
            data=item["input"],
            expected=201,
        )["id"]
    baseline = install_baseline(settings, tokens, inputs["baseline"]["model"])

    with listen() as pilot_socket:
        pilot_origin = origin(pilot_socket)
    config = {
        name: {"url": settings[name], "tokenFile": str(root / (name + ".token"))}
        for name in ("reactor", "exchange")
    }
    config.update(
        {
            "echo": {"url": settings["echo"]},
            "inference": {
                "url": settings["exchange"],
                "tokenFile": str(root / "inference.token"),
            },
            "adminTokenFile": str(root / "admin.token"),
            "pilotTokenFile": str(root / "pilot.token"),
            "httpListen": pilot_origin.removeprefix("http://"),
            "receiverId": "pilot",
            "gatewayEndpointId": settings["gateway"],
            "evaluationSuiteId": suite["id"],
            "baseline": {
                "inputId": inputs["baseline"]["inputId"],
                "digest": inputs["baseline"]["model"]["digest"],
                "routeId": baseline["id"],
                "alias": baseline["modelPattern"],
                "model": baseline["targetModel"],
            },
            "candidates": {
                name: {
                    "inputId": inputs[name]["inputId"],
                    "artifact": inputs[name]["model"],
                    "servingBindingId": "default",
                    "providerBindingId": "candidate",
                }
                for name in ("candidate", "rejected", "missing")
            },
        }
    )
    config["candidates"]["mismatch"] = {
        **config["candidates"]["candidate"],
        "artifact": inputs["rejected"]["model"],
    }
    config_path = root / "pilot.json"
    config_path.write_text(json.dumps(config))
    nav_config = root / "navigator.json"
    nav_config.write_text(
        json.dumps(
            {
                "principals": [
                    {
                        "token_env": "CPU_NAV_WRITER",
                        "actor_id": "operator",
                        "workspace_ids": ["cpu-pilot"],
                        "organization_id": "cpu-demo",
                        "can_takeover": True,
                    },
                    {
                        "token_env": "CPU_NAV_VIEWER",
                        "actor_id": "viewer",
                        "workspace_ids": ["cpu-pilot"],
                        "organization_id": "other-org",
                        "can_takeover": False,
                    },
                ],
                "release_pilot_bindings": [
                    {
                        "organization_id": "cpu-demo",
                        "workspace_id": "cpu-pilot",
                        "binding_id": BINDING,
                        "connection_ref_env": "CPU_PILOT_REF",
                    }
                ],
            }
        )
    )

    def launch(stack):
        active = stack.enter_context(
            process(
                args.platform_probe,
                root / "platform",
                args.descriptor,
                args.archive,
                ROOT / "prepare-static-go.py",
                config_path,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                text=True,
            )
        )
        record = child_line(active)
        assert record["binding_id"] == BINDING and record["state"] == "RUNNING", record
        ready(pilot_origin, active)
        navigator = stack.enter_context(
            process(
                args.navigator_python,
                args.navigator / "scripts/serve-persistence.py",
                "--database",
                root / "navigator.db",
                "--principal-config",
                nav_config,
                stdout=subprocess.PIPE,
                text=True,
                env={
                    **os.environ,
                    "CPU_PILOT_REF": record["connection_ref"],
                    "CPU_NAV_WRITER": tokens["navigator"],
                    "CPU_NAV_VIEWER": tokens["viewer"],
                },
            )
        )
        address = child_line(navigator)
        api = f"http://{address['host']}:{address['port']}"
        ready(api, navigator)
        return api, record["generation"]

    def invoke(api, tool, arguments):
        result = exchange(
            api,
            f"{WORK}/{BINDING}/tools/{tool}/call",
            tokens["navigator"],
            {"arguments": arguments},
        )
        assert not result.get("isError"), result
        return result["structuredContent"]

    def submit(api, model, key):
        return invoke(
            api, "release_prepare", {"candidateId": model, "idempotencyKey": key}
        )

    def wait_status(api, release, wanted, seconds=25):
        def read():
            result = invoke(api, "release_status", {"releaseId": release["id"]})
            if result["state"] == wanted:
                return result
            if result["state"] in {"REJECTED", "NEEDS_ATTENTION"}:
                raise RuntimeError(
                    f"unexpected {result['state']}: {result.get('message')}"
                )
            return None

        return wait_for(read, seconds, wanted)

    def chat():
        result = exchange(
            pilot_origin,
            "/v1/chat/completions",
            tokens["pilot"],
            {
                "model": "release-pilot",
                "messages": [{"role": "user", "content": "probe"}],
            },
        )
        return result["choices"][0]["message"]["content"]

    def approval_for(release):
        return {
            "routeVersion": release["expectedRouteVersion"],
            "snapshot": release["approvalSnapshot"],
        }

    def admin(release, action="", payload=None, expected=200, token=None):
        return exchange(
            pilot_origin,
            f"/admin/releases/{release['id']}{action}",
            token or tokens["admin"],
            payload,
            expected=expected,
        )

    with contextlib.ExitStack() as live:
        api, generation = launch(live)
        tools = exchange(api, f"{WORK}/{BINDING}/tools", tokens["navigator"])["tools"]
        assert {t["id"]: t["readOnly"] for t in tools} == {
            "release_prepare": False,
            "release_status": True,
        }
        assert exchange(api, WORK, tokens["viewer"]) == {"items": []}
        for model in ("rejected", "missing", "mismatch"):
            rejected = wait_status(api, submit(api, model, f"{model}-gate"), "REJECTED")
            assert not rejected.get("deploymentId"), rejected
        item = submit(api, "candidate", "rollback-demo")
        candidate = wait_status(api, item, "AWAITING_APPROVAL")
        assert (
            candidate["evaluation"]["score"] == 0.95
            and candidate["baselineAssessment"]["score"] == 0.90
        )
        assert chat() == "baseline"
        admin(
            candidate,
            "/approve",
            approval_for(candidate),
            expected=401,
            token=tokens["pilot"],
        )
        admin(
            candidate,
            "/approve",
            {**approval_for(candidate), "routeVersion": 999},
            expected=409,
        )
        assert (
            submit(api, "candidate", "rollback-demo")["deploymentId"]
            == candidate["deploymentId"]
        )
    # Stop and reactivate the Platform-owned process before approval.
    with contextlib.ExitStack() as live:
        api, next_generation = launch(live)
        assert next_generation > generation
        candidate = wait_status(api, candidate, "AWAITING_APPROVAL")
        assert chat() == "baseline"
        approved = admin(candidate, "/approve", approval_for(candidate))
        assert (
            admin(candidate, "/approve", approval_for(candidate))[
                "switchedRouteVersion"
            ]
            == approved["switchedRouteVersion"]
        )
        assert chat() == "candidate"
        (root / "candidate-outage").touch()
        started = time.monotonic()
        rolled_back = wait_status(api, candidate, "ROLLED_BACK", 30)
        elapsed = round(time.monotonic() - started, 2)
        assert elapsed < 30 and chat() == "baseline"
        (root / "candidate-outage").unlink()
        next_item = wait_status(
            api, submit(api, "candidate", "successful-demo"), "AWAITING_APPROVAL"
        )
        admin(next_item, "/approve", approval_for(next_item))
        released = wait_status(api, next_item, "RELEASED", 75)
        assert chat() == "candidate"
        run = exchange(
            settings["echo"],
            f"/api/v1/evaluation-inputs/{inputs['feedback']['inputId']}/actions/evaluate",
            data={
                "suiteId": suite["id"],
                "engineBindingId": "exact-match-plugin",
            },
            expected=201,
        )
        ids = [
            exchange(
                settings["echo"],
                f"/api/v1/evaluation-runs/{run['id']}/annotations",
                data={
                    "sampleIndex": i,
                    "reviewer": "cpu-demo-reviewer",
                    "correctedOutput": "A" if i % 2 else "B",
                },
                expected=201,
            )["id"]
            for i in range(1, 6)
        ]
        payload = {"runId": run["id"], "annotationIds": ids}
        admin(released, "/feedback", payload, expected=202)

        def feedback():
            result = admin(released).get("feedback", {})
            return result if result.get("preparationId") else None

        receipt = wait_for(feedback, 30, "Catalyst feedback")
        admin(released, "/feedback", payload, expected=202)
        assert feedback() == receipt
        prep = exchange(
            settings["catalyst"], f"/api/v1/preparations/{receipt['preparationId']}"
        )
        assert prep["state"] == "MAPPED" and prep.get("publishedVersionId") is None
        deployments = exchange(
            settings["reactor"], "/api/v1/deployments", tokens["reactor"]
        )
        assert len(deployments) == 3, deployments
        return {
            "mode": "SIMULATED",
            "platformGenerations": [generation, next_generation],
            "tools": sorted(t["id"] for t in tools),
            "baselineScore": candidate["baselineAssessment"]["score"],
            "candidateScore": candidate["evaluation"]["score"],
            "blockedCandidates": 3,
            "deploymentsIncludingBaseline": 3,
            "rollbackSeconds": elapsed,
            "rollbackRequestId": rolled_back["lastProbeRequestId"],
            "releaseId": released["id"],
            "feedbackSamples": len(ids),
            "preparationId": receipt["preparationId"],
            "feedbackDigest": receipt["digest"],
            "feedbackState": prep["state"],
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in (
        "navigator",
        "platform-probe",
        "descriptor",
        "archive",
        "catalyst-python",
        "echo-python",
        "exchange-python",
        "reactor-python",
        "navigator-python",
        "report",
    ):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    # Keep the virtualenv executable path intact even when python is a symlink.
    for name, value in vars(args).items():
        setattr(args, name, value.absolute())
    with (
        tempfile.TemporaryDirectory(prefix="cyrene-release-cpu-") as folder,
        contextlib.ExitStack() as stack,
    ):
        report = run(args, Path(folder), stack)
    result = json.dumps(report, indent=2, sort_keys=True) + "\n"
    args.report.write_text(result)
    sys.stdout.write(result)


if __name__ == "__main__":
    main()
