import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("package_linux", Path(__file__).with_name("package-linux.py"))
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class LinuxPackageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binaries = self.root / "bin"
        self.binaries.mkdir()
        for name in packager.RESOURCES:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            if name in ("drivers", "web", "optimizer/native/bundle"):
                path.mkdir(parents=True, exist_ok=True)
            else:
                path.write_text(name)
        (self.root / "drivers/BUNDLED_SOURCE.json").write_text(json.dumps({"drivers": ["fixture"]}))
        (self.root / "drivers/fixture.lua").write_text("-- pinned fixture")
        (self.root / "web/index.html").write_text("<title>FTW</title>")
        (self.root / "optimizer/native/bundle/manifest.json").write_text("{}")
        for name in ("ftw", "ftw-backup"):
            (self.binaries / name).write_bytes(b"\x7fELF\x02\x01" + bytes(12) + (62).to_bytes(2, "little"))

    def build(self):
        return packager.package(self.root, self.binaries, self.root / "release", "amd64")

    def test_archive_has_runtime_backup_service_and_checksums(self):
        archive = self.build()
        with tarfile.open(archive) as tar:
            for name in ("ftw", "ftw-backup", "web/index.html", "drivers/fixture.lua",
                         "optimizer/native/bundle/manifest.json", "deploy/ftw.service", "LICENSE"):
                self.assertTrue(tar.getmember(name).isfile(), name)
            self.assertEqual(tar.getmember("ftw").mode, 0o755)
            self.assertEqual(tar.getmember("forty-two-watts").linkname, "ftw")
            self.assertFalse(any(name.startswith(("data/", "bin/")) for name in tar.getnames()))
        legacy = archive.with_name("forty-two-watts-linux-amd64.tar.gz")
        self.assertEqual(archive.read_bytes(), legacy.read_bytes())
        for path in (archive, legacy):
            checksum = path.with_name(path.name + ".sha256").read_text()
            self.assertEqual(checksum, f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n")

    def test_checkout_times_do_not_change_the_archive(self):
        first = self.build().read_bytes()
        for path in self.root.rglob("*"):
            os.utime(path, (123456, 123456))
        self.assertEqual(self.build().read_bytes(), first)

    def test_wrong_architecture_does_not_replace_a_good_archive(self):
        first = self.build().read_bytes()
        (self.binaries / "ftw").write_bytes(b"\x7fELF\x02\x01" + bytes(12) + (183).to_bytes(2, "little"))
        with self.assertRaisesRegex(ValueError, "Linux amd64"):
            self.build()
        self.assertEqual((self.root / "release/ftw-linux-amd64.tar.gz").read_bytes(), first)

    def test_missing_pinned_driver_fails(self):
        (self.root / "drivers/fixture.lua").unlink()
        with self.assertRaisesRegex(ValueError, "missing bundled driver"):
            self.build()

    def test_external_resource_symlink_fails(self):
        (self.root / "web/private").symlink_to("/etc/passwd")
        with self.assertRaisesRegex(ValueError, "unexpected resource link"):
            self.build()
        self.assertFalse((self.root / "release/ftw-linux-amd64.tar.gz").exists())


if __name__ == "__main__":
    unittest.main()
