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

    def test_drafts_do_not_block_recovery(self):
        release_order.check("v0.131.0-beta.1", [[
            {"tag_name": "v0.131.0-beta.2", "draft": True},
        ]])

    def test_bad_release_list_fails_closed(self):
        with self.assertRaises(ValueError):
            release_order.check("v0.131.0-beta.1", [{"tag_name": "v0.130.4"}])


if __name__ == "__main__":
    unittest.main()
