#!/usr/bin/env python3
"""Package the two-binary 2.x layout for an old-line repair release."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import tempfile


MACHINES = {"amd64": 62, "arm64": 183}


def package(root, output, arch, version):
    if not re.fullmatch(r"v2\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("legacy packages require a stable v2.x tag")
    binaries = {name: root / "bin" / f"{name}-linux-{arch}"
                for name in ("ftw", "ftw-backup")}
    for name, path in binaries.items():
        with path.open("rb") as source:
            header = source.read(20)
        if (len(header) != 20 or header[:6] != b"\x7fELF\x02\x01"
                or int.from_bytes(header[18:20], "little") != MACHINES[arch]):
            raise ValueError(f"{name} is not a Linux {arch} ELF binary")
    resources = ["drivers", "web", "config.example.yaml", "LICENSE", "NOTICE"]
    # Read resources from the immutable tag, never from the workflow checkout.
    # Early 2.x used Python; later 2.x shipped a compiled worker bundle.
    if (root / "optimizer/native/bundle/manifest.json").is_file():
        resources.append("optimizer/native/bundle")
    else:
        resources.extend(["optimizer/pyproject.toml", "optimizer/ftw_optimizer"])
    for name in ("LICENSING.md", "THIRD-PARTY-NOTICES.txt"):
        if (root / name).is_file():
            resources.append(name)
    for name in resources:
        if not (root / name).exists():
            raise ValueError(f"missing 2.x runtime resource: {name}")
    pin = json.loads((root / "drivers/BUNDLED_SOURCE.json").read_text())
    for driver in pin["drivers"]:
        if not (root / "drivers" / f"{driver}.lua").is_file():
            raise ValueError(f"missing bundled driver: {driver}")

    def normalized(info):
        if info.issym() or info.islnk():
            raise ValueError(f"unexpected resource link: {info.name}")
        info.uid = info.gid = info.mtime = 0
        info.uname = info.gname = ""
        info.pax_headers = {}
        info.mode = 0o755 if info.isdir() or info.mode & 0o111 else 0o644
        return info

    output.mkdir(parents=True, exist_ok=True)
    archive = output / f"ftw-linux-{arch}.tar.gz"
    # Build privately so a failed package cannot replace an earlier good one.
    with tempfile.TemporaryDirectory(dir=output) as stage:
        pending = Path(stage) / archive.name
        with pending.open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as tar:
                    for name, path in binaries.items():
                        info = normalized(tar.gettarinfo(str(path), name))
                        info.mode = 0o755
                        with path.open("rb") as binary:
                            tar.addfile(info, binary)
                    alias = tarfile.TarInfo("forty-two-watts")
                    alias.type = tarfile.SYMTYPE
                    alias.linkname = "ftw"
                    alias.mode = 0o777
                    tar.addfile(alias)
                    for name in resources:
                        tar.add(root / name, arcname=name, filter=normalized)
        os.replace(pending, archive)
    legacy = output / f"forty-two-watts-linux-{arch}.tar.gz"
    shutil.copyfile(archive, legacy)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    for path in (archive, legacy):
        path.with_name(path.name + ".sha256").write_text(f"{digest}  {path.name}\n")
    return archive


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arch", choices=MACHINES)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, default=Path("release"))
    parser.add_argument("--version", default=os.environ.get("VERSION", ""))
    args = parser.parse_args()
    try:
        archive = package(args.root.resolve(), args.output, args.arch, args.version)
    except (OSError, ValueError, KeyError) as error:
        raise SystemExit(str(error)) from error
    print(f"built {archive} ({archive.stat().st_size} bytes)")
