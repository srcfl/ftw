"""The app-pin watch: the comparison, its exit codes and the issue steps.

Nothing here touches GitHub. The workflow's shell steps run against a stub gh.
"""
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest.mock import patch

import ha_app_pin


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/ha-app-pin.yml"
NOW = datetime(2026, 11, 10, 12, 0, tzinfo=timezone.utc)
ASSETS = ["ftw-native-release.json", "ftw-linux-amd64.tar.gz", "ftw-linux-arm64.tar.gz"]


def release(tag, age, draft=False, assets=ASSETS):
    published = (NOW - age).strftime("%Y-%m-%dT%H:%M:%SZ")
    return {"tag_name": tag, "draft": draft, "published_at": published,
            "assets": [{"name": name} for name in assets]}


def config(version):
    return f'name: FTW (0.x beta)\nversion: "{version}"\nslug: ftw-0x-beta\n'


def days(count):
    return timedelta(days=count)


def minutes(count):
    return timedelta(minutes=count)


class CompareTest(unittest.TestCase):
    def test_november_2026_is_reported_with_every_missed_beta(self):
        pages = [[release("v0.163.0-beta.1", timedelta(hours=44)),
                  release("v0.162.0-beta.1", timedelta(hours=45))],
                 [release("v0.161.0-beta.1", timedelta(hours=49)),
                  release("v0.160.0-beta.1", timedelta(hours=52)),
                  release("v0.159.0-beta.1", days(5))]]
        behind, report = ha_app_pin.check(pages, config("0.159.0-beta.1"), NOW)
        self.assertTrue(behind)
        self.assertIn("app pin:      v0.159.0-beta.1", report)
        self.assertIn("newest beta:  v0.163.0-beta.1", report)
        self.assertIn("4 beta(s) behind. The oldest has waited 52 hours.", report)
        for minor in (160, 161, 162, 163):
            self.assertIn(f"  v0.{minor}.0-beta.1", report)

    def test_current_pin_is_quiet(self):
        pages = [[release("v0.163.0-beta.1", days(2)), release("v0.162.0-beta.1", days(2))]]
        behind, report = ha_app_pin.check(pages, config("0.163.0-beta.1"), NOW)
        self.assertFalse(behind)
        self.assertIn("The app pins the newest beta that has had time to land.", report)

    def test_a_fresh_beta_gets_time_to_land(self):
        pages = [[release("v0.164.0-beta.1", minutes(20)), release("v0.163.0-beta.1", days(2))]]
        behind, report = ha_app_pin.check(pages, config("0.163.0-beta.1"), NOW)
        self.assertFalse(behind)
        self.assertIn("v0.164.0-beta.1 is 20 min old and may still be on its way.", report)

    def test_a_fresh_beta_does_not_hide_an_older_miss(self):
        pages = [[release("v0.164.0-beta.1", minutes(20)), release("v0.163.0-beta.1", days(2)),
                  release("v0.162.0-beta.1", days(3))]]
        behind, report = ha_app_pin.check(pages, config("0.162.0-beta.1"), NOW)
        self.assertTrue(behind)
        self.assertIn("1 beta(s) behind", report)
        self.assertIn("  v0.163.0-beta.1", report)

    def test_the_grace_period_ends(self):
        for age, expected in ((ha_app_pin.GRACE - minutes(1), False), (ha_app_pin.GRACE, True)):
            pages = [[release("v0.164.0-beta.1", age), release("v0.163.0-beta.1", days(2))]]
            with self.subTest(age=age):
                self.assertEqual(ha_app_pin.check(pages, config("0.163.0-beta.1"), NOW)[0], expected)

    def test_only_published_new_line_betas_count(self):
        pages = [[release("v0.200.0-beta.1", days(1), draft=True),  # a draft
                  release("v0.164.0", days(1)),                     # a stable
                  release("v0.130.9-beta.1", days(1)),              # the old line
                  release("v3.8.0-beta.1", days(1)),
                  release("drivers-beta", days(1)),
                  release("v0.163.0-beta.1", days(2))]]
        behind, report = ha_app_pin.check(pages, config("0.163.0-beta.1"), NOW)
        self.assertFalse(behind)
        self.assertIn("newest beta:  v0.163.0-beta.1", report)

    def test_versions_compare_as_numbers(self):
        pages = [[release("v0.163.10-beta.1", days(1)), release("v0.163.9-beta.2", days(2))]]
        self.assertTrue(ha_app_pin.check(pages, config("0.163.9-beta.2"), NOW)[0])
        self.assertFalse(ha_app_pin.check(pages, config("0.163.10-beta.1"), NOW)[0])

    def test_a_beta_the_app_cannot_pin_is_named(self):
        pages = [[release("v0.164.0-beta.1", days(1), assets=["ftw-linux-amd64.tar.gz"]),
                  release("v0.163.0-beta.1", days(2))]]
        behind, report = ha_app_pin.check(pages, config("0.163.0-beta.1"), NOW)
        self.assertTrue(behind)
        self.assertIn("v0.164.0-beta.1  (no ftw-native-release.json: the app cannot pin it)", report)

    def test_input_that_cannot_be_judged_is_an_error(self):
        good = [[release("v0.163.0-beta.1", days(2))]]
        cases = {
            "no beta in the list": ([[release("drivers-beta", days(1))]], config("0.163.0-beta.1")),
            "an API error object": ({"message": "Not Found"}, config("0.163.0-beta.1")),
            "an entry without a tag": ([[{"draft": False}]], config("0.163.0-beta.1")),
            "no publication time": ([[{"tag_name": "v0.163.0-beta.1"}]], config("0.163.0-beta.1")),
            "an empty config": (good, ""),
            "an old-line pin": (good, config("3.8.0-beta.1")),
        }
        for name, (pages, text) in cases.items():
            with self.subTest(name), self.assertRaises(ValueError):
                ha_app_pin.check(pages, text, NOW)


class ExitCodeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.pages = Path(self.tmp.name) / "pages.json"
        self.config = Path(self.tmp.name) / "config.yaml"
        self.pages.write_text(json.dumps([[release("v0.163.0-beta.1", days(2))]]))

    def run_check(self, *args):
        return subprocess.run([sys.executable, str(Path(ha_app_pin.__file__)), *args],
                              capture_output=True, text=True)

    def test_current_is_0_and_behind_is_1(self):
        self.config.write_text(config("0.163.0-beta.1"))
        current = self.run_check(str(self.pages), str(self.config))
        self.assertEqual((current.returncode, current.stderr), (0, ""))
        self.config.write_text(config("0.159.0-beta.1"))
        behind = self.run_check(str(self.pages), str(self.config))
        self.assertEqual(behind.returncode, 1)
        self.assertIn("1 beta(s) behind", behind.stdout)

    def test_anything_that_cannot_be_judged_is_2_never_1(self):
        self.config.write_text(config("0.163.0-beta.1"))
        self.assertEqual(self.run_check().returncode, 2)
        self.assertEqual(self.run_check(str(self.pages), str(self.config) + ".missing").returncode, 2)
        self.pages.write_text("<html>rate limited</html>")
        self.assertEqual(self.run_check(str(self.pages), str(self.config)).returncode, 2)
        self.pages.write_text("[]")
        self.assertEqual(self.run_check(str(self.pages), str(self.config)).returncode, 2)

    def test_a_bug_in_the_check_is_2_never_1(self):
        self.config.write_text(config("0.163.0-beta.1"))
        with patch.object(ha_app_pin, "check", side_effect=RuntimeError("bug")), \
                patch.object(sys, "stderr"):
            self.assertEqual(ha_app_pin.main(["x", str(self.pages), str(self.config)]), 2)


def step(name):
    """The shell of one workflow step, as the runner would get it."""
    block = WORKFLOW.read_text().split(f"      - name: {name}\n", 1)[1].split("\n      - name:", 1)[0]
    return textwrap.dedent(block.split("        run: |\n", 1)[1])


class WorkflowStepTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        gh = self.work / "gh"
        gh.write_text(f"#!{sys.executable}\n" + textwrap.dedent('''\
            import json, os, pathlib, sys
            root = pathlib.Path(os.environ["TEST_ROOT"])
            args = sys.argv[1:]
            with (root / "calls").open("a") as log:
                log.write(json.dumps(args) + "\\n")
            issue = root / "open-issue"
            if args[:2] == ["issue", "list"]:
                print(issue.read_text() if issue.exists() else "null")
            elif args[:2] == ["issue", "create"]:
                issue.write_text("7")
            elif args[:2] == ["issue", "close"]:
                issue.unlink()
            elif args[:2] != ["issue", "edit"]:
                sys.exit(3)
            '''))
        gh.chmod(0o755)

    def run_step(self, name, **env):
        (self.work / "calls").write_text("")
        env = dict(os.environ, PATH=f"{self.work}:{os.environ['PATH']}", TEST_ROOT=str(self.work),
                   GH_TOKEN="test-only", RUNNER_TEMP=str(self.work),
                   GITHUB_OUTPUT=str(self.work / "output"), **env)
        result = subprocess.run(["bash", "-c", step(name)], env=env, cwd=ROOT,
                                capture_output=True, text=True)
        calls = [json.loads(line) for line in (self.work / "calls").read_text().splitlines()]
        return result, calls

    def test_behind_opens_one_issue_and_then_rewrites_it(self):
        name = "Open or update the tracking issue"
        result, calls = self.run_step(name, REPORT="app pin:      v0.159.0-beta.1", RUN_URL="https://example.invalid/run/1")
        self.assertEqual(result.returncode, 0, result.stderr)
        created = [call for call in calls if call[:2] == ["issue", "create"]]
        self.assertEqual(len(created), 1)
        body = created[0][created[0].index("--body") + 1]
        self.assertIn("```\napp pin:      v0.159.0-beta.1\n```", body)
        self.assertIn("`HA_ADDON_DISPATCH_TOKEN`", body)
        self.assertIn("From https://example.invalid/run/1", body)

        result, calls = self.run_step(name, REPORT="app pin:      v0.160.0-beta.1", RUN_URL="https://example.invalid/run/2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse([call for call in calls if call[:2] == ["issue", "create"]])
        edited = [call for call in calls if call[:2] == ["issue", "edit"]]
        self.assertEqual([call[2] for call in edited], ["7"])
        self.assertIn("v0.160.0-beta.1", edited[0][edited[0].index("--body") + 1])

    def test_caught_up_closes_the_same_issue_and_is_quiet_without_one(self):
        _, opened = self.run_step("Open or update the tracking issue", REPORT="r", RUN_URL="u")
        title = next(call for call in opened if call[:2] == ["issue", "create"])[3]
        name = "Close the tracking issue once the app has caught up"
        result, calls = self.run_step(name)
        self.assertEqual(result.returncode, 0, result.stderr)
        searched = next(call for call in calls if call[:2] == ["issue", "list"])
        self.assertIn(f'"{title}"', searched[searched.index("--search") + 1])
        self.assertEqual([call[2] for call in calls if call[:2] == ["issue", "close"]], ["7"])

        result, calls = self.run_step(name)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse([call for call in calls if call[:2] == ["issue", "close"]])

    def test_compare_step_passes_0_and_1_on_and_fails_on_anything_else(self):
        name = "Compare the app pin with the newest beta"
        (self.work / "release-pages.json").write_text(json.dumps([[release("v0.163.0-beta.1", days(2))]]))
        for version, status in (("0.163.0-beta.1", "0"), ("0.159.0-beta.1", "1")):
            (self.work / "ha-app-config.yaml").write_text(config(version))
            (self.work / "output").write_text("")
            result, _ = self.run_step(name)
            with self.subTest(version=version):
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"status={status}\n", (self.work / "output").read_text())
        (self.work / "release-pages.json").write_text("not json")
        result, _ = self.run_step(name)
        self.assertEqual(result.returncode, 2)
        self.assertIn("::error::the app pin check could not run", result.stdout)


if __name__ == "__main__":
    unittest.main()
