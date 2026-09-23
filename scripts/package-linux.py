#!/usr/bin/env python3
"""Package already-built Linux binaries with the matching runtime resources."""

import argparse
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile


RESOURCES = (
    "drivers", "web", "config.example.yaml", "state-schema.json",
    "deploy/ftw.service", "deploy/ftw-native.service",
    "LICENSE", "NOTICE", "LICENSING.md",
    "THIRD-PARTY-NOTICES.txt",
)
MACHINES = {"amd64": 62, "arm64": 183}
ENERGYPLAN_DIR = "optimizer/native/bundle"
# Use the reviewed verifier beside this helper, including when --root selects
# an older release checkout. Keep the pinned source bundle unchanged.
spec = importlib.util.spec_from_file_location(
    "energyplan_verify", Path(__file__).resolve().parents[1] / "optimizer/native/verify.py")
energyplan_verify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(energyplan_verify)


def package(root, binaries, output, arch):
    # Catch an accidentally reused host build before it reaches the release.
    for name in ("ftw", "ftw-backup", "ftw-launcher"):
        with (binaries / name).open("rb") as source:
            header = source.read(20)
        if (len(header) != 20 or header[:6] != b"\x7fELF\x02\x01"
                or int.from_bytes(header[18:20], "little") != MACHINES[arch]):
            raise ValueError(f"{name} is not a Linux {arch} ELF binary")
    pin = json.loads((root / "drivers/BUNDLED_SOURCE.json").read_text())
    for driver in pin["drivers"]:
        if not (root / "drivers" / f"{driver}.lua").is_file():
            raise ValueError(f"missing bundled driver: {driver}")
    for name in RESOURCES:
        if not (root / name).exists():
            raise ValueError(f"missing runtime resource: {name}")
    manifest = energyplan_verify.verify_bundle(root / ENERGYPLAN_DIR)
    target = f"linux-{arch}"
    selected = manifest["artifacts"][target]
    omitted = {entry["path"] for platform, entry in manifest["artifacts"].items()
               if platform != target}
    manifest["artifacts"] = {target: selected}
    manifest["files"] = {name: info for name, info in manifest["files"].items()
                         if name not in omitted}

    output.mkdir(parents=True, exist_ok=True)
    archive = output / f"ftw-linux-{arch}.tar.gz"
    # Normalize tar and gzip metadata so the same input has the same checksum
    # on a rerun, regardless of checkout times, file ownership or host OS.
    def normalized(info):
        if info.issym() or info.islnk():
            raise ValueError(f"unexpected resource link: {info.name}")
        info.uid = info.gid = info.mtime = 0
        info.uname = info.gname = ""
        info.pax_headers = {}
        info.mode = 0o755 if info.isdir() or info.mode & 0o111 else 0o644
        return info

    pending = None
    try:
        with tempfile.NamedTemporaryFile(dir=output, delete=False) as raw:
            pending = Path(raw.name)
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as tar:
                    for name in ("ftw", "ftw-backup", "ftw-launcher"):
                        info = normalized(tar.gettarinfo(str(binaries / name), name))
                        info.mode = 0o755
                        with (binaries / name).open("rb") as binary:
                            tar.addfile(info, binary)
                    alias = tarfile.TarInfo("forty-two-watts")
                    alias.type = tarfile.SYMTYPE
                    alias.linkname = "ftw"
                    alias.mode = 0o777
                    tar.addfile(alias)
                    for name in RESOURCES:
                        tar.add(root / name, arcname=name, filter=normalized)
                    for name in sorted(manifest["files"]):
                        path = f"{ENERGYPLAN_DIR}/{name}"
                        tar.add(root / path, arcname=path, filter=normalized)
                    data = (json.dumps(manifest, indent=2) + "\n").encode()
                    info = tarfile.TarInfo(f"{ENERGYPLAN_DIR}/manifest.json")
                    info.size = len(data)
                    info.mode = 0o644
                    tar.addfile(normalized(info), io.BytesIO(data))
                    state_schema = json.loads((root / "state-schema.json").read_text())["version"]
                    if not isinstance(state_schema, int) or state_schema <= 0:
                        raise ValueError("invalid state-schema.json version")
                    version = (json.dumps({"version": os.environ.get("VERSION", "dev"),
                                           "arch": arch, "state_schema": state_schema},
                                          sort_keys=True) + "\n").encode()
                    info = tarfile.TarInfo("release-version.json")
                    info.size = len(version)
                    info.mode = 0o644
                    tar.addfile(normalized(info), io.BytesIO(version))
        os.replace(pending, archive)
        pending = None
    finally:
        if pending is not None:
            pending.unlink(missing_ok=True)

    # Keep existing download names until installed users have moved over.
    legacy = output / f"forty-two-watts-linux-{arch}.tar.gz"
    shutil.copyfile(archive, legacy)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    for asset in (archive, legacy):
        asset.with_name(asset.name + ".sha256").write_text(f"{digest}  {asset.name}\n")
    return archive


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arch", choices=MACHINES)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--binaries", type=Path)
    parser.add_argument("--output", type=Path, default=Path("release"))
    args = parser.parse_args()
    root = args.root.resolve()
    try:
        result = package(root, args.binaries or root / "bin" / f"linux-{args.arch}", args.output, args.arch)
    except (OSError, ValueError, KeyError) as error:
        raise SystemExit(str(error)) from error
    print(f"built {result} ({result.stat().st_size} bytes)")
