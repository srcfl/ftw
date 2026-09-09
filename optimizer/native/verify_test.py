"""Exercise artifact corruption and accidental source disclosure boundaries."""
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

from verify import HERE, verify_bundle, host_key


class BundleBoundaryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "bundle"
        shutil.copytree(HERE / "bundle", self.root)

    def test_unsupported_host_keeps_integrity_checks(self):
        with patch("verify.platform.system", return_value="Darwin"), patch("verify.platform.machine", return_value="arm64"):
            self.assertIsNone(host_key())
            self.assertEqual(verify_bundle(self.root)["product"], "energyplan")

    def test_valid_bundle(self):
        self.assertEqual(verify_bundle(self.root)["product"], "energyplan")

    def test_linux_only_bundle(self):
        darwin = self.root / "ftw-solver-darwin-arm64"
        if darwin.exists():
            darwin.unlink()
        manifest = self.root / "manifest.json"
        data = json.loads(manifest.read_text())
        data["artifacts"].pop("darwin-arm64", None)
        data["files"].pop("ftw-solver-darwin-arm64", None)
        manifest.write_text(json.dumps(data))
        self.assertEqual(verify_bundle(self.root)["product"], "energyplan")

    def test_missing_required_linux_platform(self):
        (self.root / "ftw-solver-linux-amd64").unlink()
        manifest = self.root / "manifest.json"
        data = json.loads(manifest.read_text())
        del data["artifacts"]["linux-amd64"]
        del data["files"]["ftw-solver-linux-amd64"]
        manifest.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "required platform"):
            verify_bundle(self.root)

    def test_modified_executable(self):
        file = self.root / "ftw-solver-linux-arm64"
        data = bytearray(file.read_bytes()); data[-1] ^= 1; file.write_bytes(data)
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            verify_bundle(self.root)

    def test_unlisted_source(self):
        (self.root / "kernel.rs").write_text("private source")
        with self.assertRaisesRegex(ValueError, "Unlisted files"):
            verify_bundle(self.root)

    def test_added_source_in_manifest(self):
        manifest = self.root / "manifest.json"
        data = json.loads(manifest.read_text()); data["files"]["kernel.rs"] = {}
        manifest.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "Unexpected or missing"):
            verify_bundle(self.root)

    def test_symlink_cannot_replace_binary(self):
        file = self.root / "ftw-solver-linux-arm64"
        file.unlink(); file.symlink_to(HERE / "bundle/ftw-solver-linux-arm64")
        with self.assertRaisesRegex(ValueError, "unsafe artifact"):
            verify_bundle(self.root)

    def test_missing_notice(self):
        (self.root / "LICENSE.txt").unlink()
        with self.assertRaisesRegex(ValueError, "Missing"):
            verify_bundle(self.root)


if __name__ == "__main__":
    unittest.main()
