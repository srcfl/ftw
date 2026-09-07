"""Verify the pinned, binary-only Energyplan distribution. Standard library only."""
import argparse
import hashlib
import json
import os
import platform
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent
PLATFORMS = {"linux-arm64", "linux-amd64", "darwin-arm64"}
RUNTIME_LICENSES = {
    "Apache-2.0.txt", "BSD-2-Clause.txt", "CC-BY-SA-4.0.txt", "GCC-exception-3.1.txt",
    "GPL-2.0-only.txt", "GPL-3.0-or-later.txt", "ISC.txt", "LLVM-exception.txt",
    "MIT.txt", "NCSA.txt", "OFL-1.1.txt", "Unicode-3.0.txt",
}


def verify_bundle(root):
    manifest = json.loads((root / "manifest.json").read_text())
    if (manifest.get("schema_version") != 1 or manifest.get("product") != "energyplan"
            or manifest.get("protocol_version") != 1
            or manifest.get("source_repository") != "srcfl/energyplan"
            or not re.fullmatch(r"[0-9a-f]{40}", manifest.get("source_commit", ""))
            or not re.fullmatch(r"\d+\.\d+\.\d+", manifest.get("version", ""))):
        raise ValueError("Invalid Energyplan manifest identity")
    artifacts, files = manifest["artifacts"], manifest["files"]
    if set(artifacts) != PLATFORMS:
        raise ValueError("The bundle must contain every supported platform")
    expected = {f"ftw-solver-{name}" for name in PLATFORMS}
    expected |= {"LICENSE.txt", "THIRD-PARTY-NOTICES.txt", "rust-runtime/COPYRIGHT-library.html"}
    expected |= {f"rust-runtime/licenses/{name}" for name in RUNTIME_LICENSES}
    if set(files) != expected:
        raise ValueError("Unexpected or missing distribution file; review the binary boundary")
    for name, info in files.items():
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts:
            raise ValueError("Manifest path escapes the bundle")
        file = root / name
        if file.is_symlink() or not file.is_file() or root.resolve() not in file.resolve().parents:
            raise ValueError(f"Missing or unsafe artifact: {name}")
        data = file.read_bytes()
        if len(data) != info["bytes"] or hashlib.sha256(data).hexdigest() != info["sha256"]:
            raise ValueError(f"Artifact checksum mismatch: {name}")
    actual = {str(p.relative_to(root)) for p in root.rglob("*") if p.is_file()}
    if actual != expected | {"manifest.json"}:
        raise ValueError("Unlisted files in the binary bundle")
    for name, artifact in artifacts.items():
        if artifact["path"] != f"ftw-solver-{name}":
            raise ValueError("Unexpected executable path")
        file = root / artifact["path"]
        data = file.read_bytes()
        if name.startswith("linux-"):
            machine = 183 if name.endswith("arm64") else 62
            if data[:6] != b"\x7fELF\x02\x01" or int.from_bytes(data[18:20], "little") != machine:
                raise ValueError(f"Wrong executable architecture: {name}")
        elif data[:4] != bytes.fromhex("cffaedfe") or int.from_bytes(data[4:8], "little") != 0x100000c:
            raise ValueError(f"Wrong executable architecture: {name}")
        if os.name != "nt" and not os.access(file, os.X_OK):
            raise ValueError(f"Executable bit missing: {name}")
    return manifest


def host_key():
    machine = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(platform.machine().lower())
    key = f"{platform.system().lower()}-{machine}"
    if key not in PLATFORMS:
        raise ValueError(f"No bundled Energyplan worker for {key}")
    return key


def check_public_tree():
    repo = HERE.parents[1]
    names = subprocess.check_output(["git", "ls-files", "-z", "optimizer/native"], cwd=repo).decode().split("\0")
    allowed = {".gitattributes", "README.md", "verify.py", "verify_test.py"}
    for name in filter(None, names):
        local = str(Path(name).relative_to("optimizer/native"))
        if local not in allowed and not local.startswith("bundle/"):
            raise ValueError(f"Private source/build file in the public integration: {name}")
        if Path(name).suffix == ".rs" or Path(name).name in {"Cargo.toml", "Cargo.lock"}:
            raise ValueError(f"Rust source/build metadata must stay private: {name}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host-binary", action="store_true")
    args = parser.parse_args()
    check_public_tree()
    root = HERE / "bundle"
    manifest = verify_bundle(root)
    binary = root / manifest["artifacts"][host_key()]["path"]
    if args.host_binary:
        print(binary)
        return
    result = subprocess.run([str(binary)], input='{"type":"handshake"}\n', capture_output=True,
                            text=True, check=True, timeout=5)
    reply = json.loads(result.stdout)
    if reply.get("protocol_version") != 1 or reply.get("version") != manifest["version"]:
        raise ValueError("Worker handshake does not match the pinned version")
    print(f"Verified Energyplan {manifest['version']}: {len(manifest['artifacts'])} platforms; {host_key()} handshake passed")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
