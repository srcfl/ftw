"""Native release tags must move forward without changing legacy latest."""

import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "check_native_release_order", Path(__file__).with_name("check-native-release-order.py"))
release_order = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_order)


class NativeReleaseOrderTest(unittest.TestCase):
    def test_beta_and_stable_on_same_source_line(self):
        pages = [[
            {"tag_name": "v3.8.0", "draft": False},
            {"tag_name": "v0.130.4", "draft": False},
            {"tag_name": "v0.131.0-beta.2", "draft": False},
        ]]
        release_order.check("v0.131.0", pages)
        release_order.check("v0.131.0-beta.3", pages)

    def test_rejects_old_or_backdated_candidate(self):
        pages = [[{"tag_name": "v0.131.0-beta.2", "draft": False}]]
        for tag in ("v0.130.5", "v0.131.0-beta.1", "v0.130.4-beta.8"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release_order.check(tag, pages)

    def test_published_stable_closes_its_beta_line(self):
        pages = [[{"tag_name": "v0.131.0", "draft": False}]]
        with self.assertRaises(ValueError):
            release_order.check("v0.131.0-beta.3", pages)

    def test_newer_beta_does_not_block_tested_stable(self):
        release_order.check("v0.158.0", [[
            {"tag_name": "v0.159.0-beta.1", "draft": False},
            {"tag_name": "v0.158.0-beta.3", "draft": False},
        ]])
        release_order.check("v0.157.2", [[
            {"tag_name": "v0.157.3-beta.1", "draft": False},
            {"tag_name": "v0.157.2-beta.1", "draft": False},
        ]])
        release_order.check("v0.158.1", [[
            {"tag_name": "v0.159.0-beta.1", "draft": False},
            {"tag_name": "v0.158.0", "draft": False},
            {"tag_name": "v0.158.1-beta.1", "draft": False},
        ]])

    def test_rejects_stable_older_than_published_stable(self):
        pages = [[
            {"tag_name": "v0.159.0", "draft": False},
            {"tag_name": "v0.158.1-beta.1", "draft": False},
        ]]
        for tag in ("v0.158.1", "v0.158.0-beta.4"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release_order.check(tag, pages)

    def test_rejects_beta_older_than_published_beta(self):
        with self.assertRaises(ValueError):
            release_order.check("v0.158.1-beta.1", [[
                {"tag_name": "v0.159.0-beta.1", "draft": False},
            ]])

    def test_after_late_stable_betas_stay_above_every_release(self):
        # The updater stops at the first beta it lists (selfupdate.go
        # resolveChannel), so no beta may rank below a published release.
        pages = [[
            {"tag_name": "v0.158.0", "draft": False},
            {"tag_name": "v0.159.0-beta.1", "draft": False},
        ]]
        release_order.check("v0.159.0-beta.2", pages)
        release_order.check("v0.159.1-beta.1", pages)
        release_order.check("v0.158.0", pages)
        with self.assertRaises(ValueError):
            release_order.check("v0.158.0-beta.4", pages)

    def test_newer_stable_on_a_later_page_blocks_stable(self):
        with self.assertRaises(ValueError):
            release_order.check("v0.158.0", [
                [{"tag_name": "v0.160.0-beta.1", "draft": False}],
                [{"tag_name": "v0.159.0", "draft": False}],
            ])
        release_order.check("v0.158.0", [[
            {"tag_name": "v0.159.0", "draft": True},
            {"tag_name": "v0.159.0-beta.1", "draft": False},
        ]])

    def test_drafts_do_not_block_recovery(self):
        release_order.check("v0.131.0-beta.1", [[
            {"tag_name": "v0.131.0-beta.2", "draft": True},
        ]])

    def test_bad_release_list_fails_closed(self):
        with self.assertRaises(ValueError):
            release_order.check("v0.131.0-beta.1", [{"tag_name": "v0.130.4"}])


if __name__ == "__main__":
    unittest.main()
