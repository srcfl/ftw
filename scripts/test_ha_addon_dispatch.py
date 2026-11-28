"""Run the release handoff with GitHub fixtures, without sending a dispatch."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]
TAG = "v0.154.1-beta.1"
STABLE = "v0.154.1"
ASSETS = [f"ftw-linux-{arch}.tar.gz{suffix}"
          for arch in ("amd64", "arm64") for suffix in ("", ".sha256")]


class HandoffTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        workflow = (ROOT / ".github/workflows/native-release.yml").read_text()
        job = workflow.split("  ha-addon:\n", 1)[1].split("  discord:\n", 1)[0]
        self.script = textwrap.dedent(job.split("        run: |\n", 1)[1])
        self.release = {"tagName": TAG, "isDraft": False, "isPrerelease": True,
                        "assets": [{"name": name} for name in ASSETS + ["ftw-native-release.json"]]}
        self.receipt = {"schema": 1, "tag": TAG, "channel": "beta",
                        "sha256": {"amd64": "a" * 64, "arm64": "b" * 64}}
        gh = self.work / "gh"
        gh.write_text(f"#!{sys.executable}\n" + textwrap.dedent('''\
            import json, os, pathlib, sys
            args = sys.argv[1:]
            if args[:2] == ["release", "view"]:
                print(os.environ["TEST_RELEASE"])
            elif args[:2] == ["release", "download"]:
                dest = pathlib.Path(args[args.index("--dir") + 1])
                (dest / "ftw-native-release.json").write_text(os.environ["TEST_RECEIPT"])
            elif args[0] == "api":
                payload = pathlib.Path(args[args.index("--input") + 1]).read_text()
                pathlib.Path(os.environ["TEST_SENT"]).write_text(json.dumps({
                    "args": args, "token": os.environ["GH_TOKEN"], "payload": json.loads(payload)}))
            else:
                sys.exit(3)
            '''))
        gh.chmod(0o755)

    def run_handoff(self, token="test-dispatch-token", channel="beta", tag=TAG):
        env = dict(os.environ, PATH=f"{self.work}:{os.environ['PATH']}",
                   CHANNEL=channel, TAG=tag, GITHUB_REPOSITORY="srcfl/ftw", GH_TOKEN="test-read-token",
                   HA_ADDON_DISPATCH_TOKEN=token,
                   TEST_RELEASE=json.dumps(self.release), TEST_RECEIPT=json.dumps(self.receipt),
                   TEST_SENT=str(self.work / "sent.json"))
        result = subprocess.run(["bash", "-c", self.script], env=env,
                                capture_output=True, text=True)
        sent = self.work / "sent.json"
        return result, json.loads(sent.read_text()) if sent.exists() else None

    def test_published_beta_requests_one_pin_with_scoped_token(self):
        result, sent = self.run_handoff()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sent["payload"], {"event_type": "ftw-native-release",
                         "client_payload": {"tag": TAG, "channel": "beta"}})
        self.assertEqual(sent["token"], "test-dispatch-token")
        self.assertIn("repos/srcfl/home-assistant-addons/dispatches", sent["args"])
        self.assertNotIn("test-dispatch-token", result.stdout + result.stderr)

    def test_missing_token_keeps_publication_successful(self):
        result, sent = self.run_handoff(token="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("pin the Home Assistant app manually", result.stdout)
        self.assertIsNone(sent)

    def test_draft_or_missing_package_never_dispatches(self):
        self.release["isDraft"] = True
        result, sent = self.run_handoff()
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(sent)
        self.release["isDraft"] = False
        self.release["assets"].pop(0)
        result, sent = self.run_handoff()
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(sent)

    def stable(self):
        self.release.update(tagName=STABLE, isPrerelease=False)
        self.receipt.update(tag=STABLE, channel="stable", source_beta=TAG)

    def test_published_stable_requests_its_pin_too(self):
        self.stable()
        result, sent = self.run_handoff(channel="stable", tag=STABLE)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sent["payload"], {"event_type": "ftw-native-release",
                         "client_payload": {"tag": STABLE, "channel": "stable"}})
        self.assertEqual(sent["token"], "test-dispatch-token")

    def test_stable_handoff_checks_tag_release_and_receipt(self):
        for name, change, tag in (
            ("beta tag on the stable channel", lambda: None, TAG),
            ("release marked prerelease", lambda: self.release.update(isPrerelease=True), STABLE),
            ("receipt from the beta", lambda: self.receipt.update(channel="beta"), STABLE),
        ):
            with self.subTest(name):
                self.stable()
                change()
                result, sent = self.run_handoff(channel="stable", tag=tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(sent)

    def test_unknown_channel_never_dispatches(self):
        result, sent = self.run_handoff(channel="edge")
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(sent)

    def test_wrong_receipt_never_dispatches(self):
        self.receipt["tag"] = "v0.154.0-beta.1"
        result, sent = self.run_handoff()
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(sent)


if __name__ == "__main__":
    unittest.main()
