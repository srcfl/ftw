"""Release pages must describe the candidate and retain the schema marker."""

import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "release_notes", Path(__file__).with_name("write-native-release-notes.py"))
release_notes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_notes)


class ReleaseNotesTest(unittest.TestCase):
    def receipt(self, tag="v0.148.0-beta.1", source_beta=""):
        return dict(tag=tag, source_beta=source_beta, commit="a" * 40, state_schema=7)

    def test_candidate_section_keeps_user_steps_and_leaves_out_other_versions(self):
        changelog = """# Changelog

## 0.149.0
- Future change.

## 0.148.0
### Minor Changes
- 6335dbc: Add hourly statistics.
  Older consents stay inactive.

## Before enabling statistics
Read the new policy.

## 0.147.2
- Old charging change.
"""
        notes = release_notes.build_notes(self.receipt(), changelog)
        self.assertIn("## Changes in this beta", notes)
        self.assertIn("- Add hourly statistics.\n  Older consents stay inactive.", notes)
        self.assertIn("## Before enabling statistics\nRead the new policy.", notes)
        self.assertNotIn("Future change", notes)
        self.assertNotIn("Old charging change", notes)
        self.assertNotIn("6335dbc:", notes)
        self.assertIn("/blob/v0.148.0-beta.1/CHANGELOG.md", notes)
        self.assertIn("<!-- ftw-state-schema-v2:7 -->", notes)

    def test_stable_uses_tested_beta_source_changelog(self):
        notes = release_notes.build_notes(
            self.receipt("v0.148.0", "v0.148.0-beta.1"), "## 0.148.0\n- Add statistics.")
        self.assertIn("## Changes in this release", notes)
        self.assertIn("Promoted from site-tested [v0.148.0-beta.1]", notes)

    def test_missing_or_empty_candidate_fails_instead_of_publishing_generic_notes(self):
        for changelog in ("## 0.147.2\n- Old change.", "## 0.148.0\n",
                          "## 0.148.0\n### Minor Changes\n<!-- marker -->\n"):
            with self.subTest(changelog=changelog), self.assertRaises(ValueError):
                release_notes.build_notes(self.receipt(), changelog)

    UNRELEASED = """# Changelog

## 0.149.1
### Patch Changes
- 1111111: Escape quotes on the setup page.

## 0.149.0
### Patch Changes
- 2222222: Bundle a faster planner.
### Minor Changes
- 3333333: Show why a boost was refused.

## 0.148.0
- Already shipped.
"""

    def test_beta_after_an_unreleased_version_lists_its_changes_too(self):
        notes = release_notes.build_notes(
            self.receipt("v0.149.1-beta.1"), self.UNRELEASED, published={"0.148.0"})
        self.assertIn("## Changes in this beta\n\n### Patch Changes\n- Escape quotes", notes)
        self.assertIn("## From 0.149.0, which had no release of its own\n\n"
                      "### Patch Changes\n- Bundle a faster planner.", notes)
        self.assertIn("### Minor Changes\n- Show why a boost was refused.", notes)
        self.assertLess(notes.index("Escape quotes"), notes.index("From 0.149.0"))
        self.assertNotIn("Already shipped", notes)
        self.assertNotIn("2222222:", notes)

    def test_extra_versions_need_a_known_published_base(self):
        for published in (None, set(), {"0.140.0"}):
            with self.subTest(published=published):
                notes = release_notes.build_notes(
                    self.receipt("v0.149.1-beta.1"), self.UNRELEASED, published=published)
                self.assertIn("Escape quotes", notes)
                self.assertNotIn("faster planner", notes)
                self.assertNotIn("From 0.149.0", notes)

    def test_stable_and_repeat_beta_keep_one_section(self):
        stable = release_notes.build_notes(
            self.receipt("v0.149.1", "v0.149.1-beta.1"), self.UNRELEASED, published={"0.148.0"})
        again = release_notes.build_notes(
            self.receipt("v0.149.1-beta.2"), self.UNRELEASED, published={"0.149.1", "0.148.0"})
        for notes in (stable, again):
            self.assertIn("Escape quotes", notes)
            self.assertNotIn("faster planner", notes)

    def test_published_versions_reads_release_pages(self):
        pages = [[{"tag_name": "v0.148.0-beta.2", "draft": False},
                  {"tag_name": "v0.147.0", "draft": False},
                  {"tag_name": "v0.149.0-beta.1", "draft": True},
                  {"tag_name": "v3.8.0", "draft": False}],
                 [{"tag_name": "drivers-beta", "draft": False}]]
        self.assertEqual(release_notes.published_versions(pages), {"0.148.0", "0.147.0"})
        for bad in ({}, [{}], [[{"draft": False}]]):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                release_notes.published_versions(bad)

    def test_workflow_keeps_note_writer_when_promoting_older_beta(self):
        workflow = (Path(__file__).parents[1] / ".github/workflows/native-release.yml").read_text()
        copy = workflow.index('cp scripts/write-native-release-notes.py "${RUNNER_TEMP}/write-native-release-notes.py"')
        checkout = workflow.index('git checkout --detach "${beta_commit}"')
        run = workflow.index('python3 "${RUNNER_TEMP}/write-native-release-notes.py"')
        self.assertLess(copy, checkout)
        self.assertLess(checkout, run)
        self.assertIn('--notes-file "${release_dir}/notes.md"', workflow)
        pages = workflow.index('> "${RUNNER_TEMP}/native-release-pages.json"')
        published = workflow.index('--published "${RUNNER_TEMP}/native-release-pages.json"')
        self.assertLess(pages, published)
        self.assertLess(run, published)
        self.assertNotIn('/releases/latest', workflow)
        self.assertIn('--draft=false --latest=false --verify-tag', workflow)
        self.assertNotIn('--latest=true', workflow)


if __name__ == "__main__":
    unittest.main()
