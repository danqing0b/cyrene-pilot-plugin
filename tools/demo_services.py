# SPDX-License-Identifier: Apache-2.0
"""CPU-only Product assembly. Only Reactor's engine and Exchange providers are simulated."""

import json
import os
import socket
import sys
from pathlib import Path
from uuid import UUID

import uvicorn


def echo(root, settings):
    from cyrene_echo.api import create_app
    from cyrene_echo.engine import EchoArtifactPlane
    from cyrene_plugin_runtime import serve
    from exact_match_evaluator import ExactMatchEvaluationRunner

    runner, ref = serve(
        ExactMatchEvaluationRunner(), "evaluation.runner.v1", "1", "127.0.0.1:0"
    )
    os.environ["CYRENE_EVALUATION_RUNNER_CONNECTION_REF"] = ref
    plane = EchoArtifactPlane(root / "artifacts")
    descriptions = {}
    for name, correct, total, sample_set, model_name in [
        ("baseline", 90, 100, "benchmark", "baseline"),
        ("candidate", 95, 100, "benchmark", "candidate"),
        ("rejected", 85, 100, "benchmark", "rejected"),
        ("missing", 95, 100, "benchmark", "missing"),
        ("feedback", 0, 5, "review", "candidate"),
    ]:
        model = plane.publish_bytes(f"simulated:{model_name}".encode(), kind="model")
        rows = []
        for i in range(total):
            label = "A" if i % 2 == 0 else "B"
            value = label if i < correct else "C"
            if name == "missing" and i == total - 1:
                value = None
            rows.append(
                {
                    "sampleId": f"{sample_set}-{i:03}",
                    "instruction": f"Classify {sample_set} {i}",
                    "reference": label,
                    "actual": value,
                }
            )
        raw = ("\n".join(map(json.dumps, rows)) + "\n").encode()
        data = plane.publish_bytes(raw, kind="dataset")
        descriptions[name] = {
            "model": model.model_dump(mode="json", exclude_none=True),
            "input": {
                "sourceRef": {
                    "uri": f"cyrene://release-cpu/{name}",
                    "id": name,
                    "resourceVersion": 1,
                },
                "artifact": data.model_dump(mode="json", exclude_none=True),
                "format": "CYRENE_REFERENCE_ACTUAL_JSONL_V1",
                "contentRefs": [f"cyrene://release-cpu/{sample_set}"],
                "targetDatasetVersion": f"catalyst://release-cpu/{sample_set}/1",
                "targetPackageArtifact": model.model_dump(
                    mode="json", exclude_none=True
                ),
            },
        }
    (root / "inputs.json").write_text(json.dumps(descriptions))
    app = create_app(
        database_path=root / "echo.db",
        artifact_root=root / "artifacts",
        catalyst_url=settings["catalyst"],
    )
    return app, runner


def catalyst(root):
    from cyrene_catalyst.api import create_app
    from cyrene_plugin_runtime import serve
    from dataset_preparation import DatasetPreparationPlugin

    runner, ref = serve(
        DatasetPreparationPlugin(), "dataset.preparation.v1", "1", "127.0.0.1:0"
    )
    os.environ["CYRENE_DATASET_PREPARATION_CONNECTION_REF"] = ref
    return create_app(
        database_path=root / "catalyst.db", artifact_root=root / "artifacts"
    ), runner


def reactor(root, settings):
    from cyrene_reactor_product.api import create_app
    from cyrene_reactor_product.domain import EngineHandle, EngineObservation
    from cyrene_reactor_product.exchange_handoff import (
        ExchangeHandoff,
        ExchangeReceiverConfiguration,
    )

    class FixedEngine:
        def __init__(self):
            self.running = {}

        def prepare(self, deployment_id):
            return EngineHandle(
                execution_ref=str(deployment_id),
                endpoint_url=settings["exchange"] + "/v1",
            )

        def start(self, deployment_id, model, *, node_ref=None):
            by_digest = json.loads((root / "inputs.json").read_text())
            label = next(
                name
                for name in ("baseline", "candidate", "rejected", "missing")
                if by_digest[name]["model"]["digest"] == model.digest
            )
            self.running[deployment_id] = model.digest
            return EngineHandle(
                execution_ref=str(deployment_id),
                endpoint_url=settings["exchange"] + "/v1",
                model=label,
            )

        def inspect(self, execution_ref, endpoint_url, deployment_id, model_digest):
            return EngineObservation(
                ready=self.running.get(deployment_id) == model_digest,
                detail="CPU fixture",
            )

        def stop(self, execution_ref, endpoint_url, deployment_id, model_digest):
            self.running.pop(deployment_id, None)
            return EngineObservation(ready=False, detail="CPU fixture stopped")

    receiver = ExchangeHandoff(
        ExchangeReceiverConfiguration(
            receiver_id="pilot",
            control_url=settings["exchange"],
            credential_file=root / "exchange.token",
            allowed_binding_ids=frozenset({"baseline", "candidate"}),
        ),
        settings["reactor"],
    )
    return create_app(
        database_path=root / "reactor.db",
        engine=FixedEngine(),
        credential_file=root / "reactor.token",
        exchange_receivers={"pilot": receiver},
    ), None


def exchange(root, settings):
    from cyrene_exchange.capabilities import ProviderChunk
    from cyrene_exchange_product.domain import (
        GatewayEndpoint,
        ProductPrincipal,
        utc_now,
    )
    from cyrene_exchange_product.server import (
        OperatorBindingResolver,
        ProviderUnavailableError,
        build_product_app,
    )
    from cyrene_exchange_product.store import ExchangeStore

    class FixedProvider:
        def __init__(self, model):
            self.model = model

        def complete(self, request, *, cancel_event):
            if request.model != self.model or (
                self.model == "candidate" and (root / "candidate-outage").exists()
            ):
                raise ProviderUnavailableError("simulated provider unavailable")
            return (
                ProviderChunk(role="assistant", delta=self.model, finish_reason="stop"),
            )

    db = root / "exchange.db"
    endpoint = UUID(settings["gateway"])
    store = ExchangeStore(db)
    now = utc_now()
    store.save_endpoint(
        GatewayEndpoint(
            id=endpoint,
            name="Isolated CPU pilot",
            public_base_url=settings["exchange"],
            auth_policy_ref="policy://cpu-demo",
            created_at=now,
            updated_at=now,
            resource_version=1,
        )
    )
    store.close()
    control = (root / "exchange.token").read_text()
    inference = (root / "inference.token").read_text()
    principal = ProductPrincipal("operator", "cpu-pilot", "cred://operator")
    return build_product_app(
        database_path=db,
        endpoint_id=endpoint,
        resolver=OperatorBindingResolver(
            {name: FixedProvider(name) for name in ("baseline", "candidate")}
        ),
        control_credentials={control: principal},
        control_only_credentials={control: principal},
        gateway_credentials={
            inference: ProductPrincipal("probe", "cpu-pilot", "cred://probe")
        },
        allowed_binding_ids=frozenset({"baseline", "candidate"}),
    ), None


def main():
    service, directory, fd = sys.argv[1:]
    root = Path(directory)
    settings = json.loads((root / "origins.json").read_text())
    if service == "catalyst":
        app, runner = catalyst(root)
    else:
        app, runner = {"echo": echo, "reactor": reactor, "exchange": exchange}[service](
            root, settings
        )
    try:
        uvicorn.Server(uvicorn.Config(app, log_level="warning")).run(
            sockets=[socket.socket(fileno=int(fd))]
        )
    finally:
        if runner is not None:
            runner.stop(grace=None).wait()


if __name__ == "__main__":
    main()
