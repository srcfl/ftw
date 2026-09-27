import hashlib
import importlib.util
import json
from pathlib import Path
import tarfile
import tempfile
import unittest


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


packager = load("package_legacy_linux", "package-legacy-linux.py")
guard = load("check_stable_release", "check-stable-release.py")


class LegacyLinuxPackageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name, data in {
            "drivers/BUNDLED_SOURCE.json": json.dumps({"drivers": ["fixture"]}),
            "drivers/fixture.lua": "-- pinned driver",
            "web/index.html": "<title>FTW</title>",
            "config.example.yaml": "drivers: []",
            "LICENSE": "tag license", "NOTICE": "tag notice",
            "optimizer/pyproject.toml": "[project]",
            "optimizer/ftw_optimizer/__init__.py": "# tag optimizer",
        }.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(data)
        (self.root / "bin").mkdir()
        for arch, machine in packager.MACHINES.items():
            for name in ("ftw", "ftw-backup"):
                (self.root / "bin" / f"{name}-linux-{arch}").write_bytes(
                    b"\x7fELF\x02\x01" + bytes(12) + machine.to_bytes(2, "little"))

    def test_both_architectures_supply_all_upload_assets_from_python_tag(self):
        output = self.root / "release"
        for arch in packager.MACHINES:
            archive = packager.package(self.root, output, arch, "v2.3.2")
            with tarfile.open(archive) as tar:
                names = tar.getnames()
                for name in ("ftw", "ftw-backup", "drivers/fixture.lua", "web/index.html",
                             "optimizer/pyproject.toml", "optimizer/ftw_optimizer/__init__.py"):
                    self.assertIn(name, names)
                self.assertEqual(tar.getmember("forty-two-watts").linkname, "ftw")
                self.assertNotIn("ftw-launcher", names)
                self.assertNotIn("deploy/ftw-native.service", names)
            legacy = output / f"forty-two-watts-linux-{arch}.tar.gz"
            self.assertEqual(archive.read_bytes(), legacy.read_bytes())
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            for path in (archive, legacy):
                self.assertEqual(path.with_name(path.name + ".sha256").read_text(), f"{digest}  {path.name}\n")
            first = archive.read_bytes()
            packager.package(self.root, output, arch, "v2.3.2")
            self.assertEqual(archive.read_bytes(), first)
        (output / "ftw-promotion-receipt.json").write_text("{}")
        guard.check_assets("v2.3.2", {
            "tagName": "v2.3.2", "isDraft": True, "isPrerelease": False,
            "publishedAt": None,
            "assets": [{"name": p.name, "state": "uploaded", "size": p.stat().st_size} for p in output.iterdir()],
        })

    def test_later_tag_keeps_its_compiled_worker_and_notices(self):
        bundle = self.root / "optimizer/native/bundle"
        bundle.mkdir(parents=True)
        (bundle / "manifest.json").write_text('{"version":"from-tag"}')
        (bundle / "ftw-solver-linux-amd64").write_bytes(b"tag worker")
        (bundle / "LICENSE").write_text("worker license")
        archive = packager.package(self.root, self.root / "release", "amd64", "v2.17.1")
        with tarfile.open(archive) as tar:
            self.assertEqual(tar.extractfile("optimizer/native/bundle/ftw-solver-linux-amd64").read(), b"tag worker")
            self.assertEqual(tar.extractfile("optimizer/native/bundle/LICENSE").read(), b"worker license")
            self.assertNotIn("optimizer/pyproject.toml", tar.getnames())

    def test_refuses_native_or_beta_tags(self):
        for version in ("v0.138.0", "v2.3.2-beta.1", "v3.8.0", "v2.03.2", ""):
            with self.subTest(version=version), self.assertRaisesRegex(ValueError, "stable v2.x"):
                packager.package(self.root, self.root / "release", "amd64", version)

    def test_refuses_missing_runtime_or_wrong_binary(self):
        (self.root / "optimizer/pyproject.toml").unlink()
        with self.assertRaisesRegex(ValueError, "missing 2.x runtime"):
            packager.package(self.root, self.root / "release", "amd64", "v2.3.2")
        (self.root / "bin/ftw-linux-amd64").write_bytes(b"wrong platform")
        with self.assertRaisesRegex(ValueError, "Linux amd64 ELF"):
            packager.package(self.root, self.root / "release", "amd64", "v2.3.2")
