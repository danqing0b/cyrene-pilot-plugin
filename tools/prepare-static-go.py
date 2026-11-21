#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Prepare an already-built, digest-locked Release Pilot executable for Platform."""

import argparse
import hashlib
import json
import os
import platform
from pathlib import Path


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package-root", type=Path, required=True)
    parser.add_argument("--runtime-root", type=Path, required=True)
    parser.add_argument("--lock-digest", required=True)
    args = parser.parse_args()
    lock = (args.package_root / "runtime.lock").read_bytes()
    manifest = json.loads((args.package_root / "plugin.manifest.json").read_bytes())
    if (
        digest(lock) != args.lock_digest
        or manifest.get("id") != "cyrene.tools.release-pilot"
    ):
        parser.error("unexpected package or runtime lock")
    executable = (args.package_root / "bin/cyrene-release-pilot").read_bytes()
    value = digest(executable)
    expected_arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if (
        platform.system() != "Linux"
        or not expected_arch
        or json.loads(lock)
        != {"os": "linux", "arch": expected_arch, "binaryDigest": value}
        or not executable.startswith(b"\x7fELF")
    ):
        parser.error("the pinned static executable does not match this host")
    name = "cyrene-release-pilot"
    path = args.runtime_root / name
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700)
    with os.fdopen(fd, "wb") as output:
        output.write(executable)
        output.flush()
        os.fsync(output.fileno())
    print(
        json.dumps(
            {
                "protocol": "cyrene.package-dependency-preparer.v1",
                "preparer": "cyrene-release-pilot-static-go-v1",
                "runtime_digest": value,
                "runtime_executable": name,
            }
        )
    )


if __name__ == "__main__":
    main()
