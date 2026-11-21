#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build the Platform Package Spec ZIP for the static Go plugin."""

import argparse
import hashlib
import json
import os
import platform
import struct
import subprocess
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def canonical(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", default="go")
    args = parser.parse_args()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or not arch:
        parser.error("package build requires Linux amd64 or arm64")
    manifest = json.loads((ROOT / "plugin.manifest.json").read_bytes())
    with tempfile.TemporaryDirectory() as temporary:
        binary = Path(temporary) / "cyrene-release-pilot"
        subprocess.run(
            [
                args.go,
                "build",
                "-mod=readonly",
                "-trimpath",
                "-buildvcs=false",
                "-o",
                binary,
                ".",
            ],
            cwd=ROOT,
            env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch},
            check=True,
        )
        executable = binary.read_bytes()
    lock = canonical({"os": "linux", "arch": arch, "binaryDigest": digest(executable)})
    entries = {
        "bin/cyrene-release-pilot": executable,
        "configuration.schema.json": (ROOT / "configuration.schema.json").read_bytes(),
        "plugin.manifest.json": (ROOT / "plugin.manifest.json").read_bytes(),
        "runtime.lock": lock,
        "LICENSE": (ROOT / "LICENSES" / "Apache-2.0.txt").read_bytes(),
    }
    content = hashlib.sha256()
    for name, data in sorted(entries.items()):
        key = name.encode()
        content.update(struct.pack(">Q", len(key)) + key + struct.pack(">Q", len(data)))
        content.update(data)
    args.output.mkdir(parents=True, exist_ok=True)
    name = f"cyrene-release-pilot-{manifest['version']}-linux-{arch}"
    archive = args.output / (name + ".zip")
    with zipfile.ZipFile(archive, "w") as package:
        for filename, data in sorted(entries.items()):
            info = zipfile.ZipInfo(filename)
            info.create_system = 3
            info.external_attr = 0o100644 << 16
            package.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED)
    digest_value = "sha256:" + content.hexdigest()
    descriptor = {
        "record_type": "package_descriptor",
        "spec_version": "0.1",
        "package": {"id": manifest["id"], "version": manifest["version"]},
        "capability": {"id": "tool.provider.v1", "interface_version": "1"},
        "implementation": {
            "entrypoint": "bin/cyrene-release-pilot",
            "artifact": {
                "status": "PUBLISHED",
                "uri": "artifact://sha256/" + content.hexdigest(),
                "digest": digest_value,
                "format": "zip",
            },
        },
        "dependencies": {
            "lock": {
                "status": "LOCKED",
                "format": "cyrene-static-go-v1",
                "ref": "runtime.lock",
                "digest": digest(lock),
            }
        },
        "configuration": {"digest": digest(entries["configuration.schema.json"])},
        "integrity": {
            "artifact_digest": digest_value,
            "archive_digest": digest(archive.read_bytes()),
        },
        "publication_status": "PUBLISHED",
    }
    path = args.output / (name + ".descriptor.json")
    path.write_bytes(canonical(descriptor))
    print(path)
    print(archive)


if __name__ == "__main__":
    main()
