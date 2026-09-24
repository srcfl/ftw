"""Native releases keep the state schema until a schema step can be taken."""

import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "check_native_schema", Path(__file__).with_name("check-native-schema.py"))
native_schema = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native_schema)


def release(tag, schema=None, draft=False):
    body = "Native FTW" + (f"\n<!-- ftw-state-schema-v2:{schema} -->\n" if schema else "")
    return {"tag_name": tag, "draft": draft, "body": body}


class NativeSchemaTest(unittest.TestCase):
    def test_same_schema_passes(self):
        pages = [[release("v3.8.0-beta.1", 5), release("v0.134.1-beta.1", 7), release("v0.134.2-beta.1", 7)]]
        native_schema.check("v0.134.3-beta.1", 7, pages)
        native_schema.check("v0.134.2", 7, pages)

    def test_schema_change_against_the_newest_older_release_fails(self):
        pages = [[release("v0.134.1-beta.1", 6)], [release("v0.134.2-beta.1", 7)]]
        with self.assertRaisesRegex(ValueError, "from 7 \\(v0.134.2-beta.1\\) to 8"):
            native_schema.check("v0.135.0-beta.1", 8, pages)

    def test_newer_drafts_and_other_lines_are_ignored(self):
        pages = [[
            release("v0.134.2-beta.1", 7),
            release("v0.136.0-beta.1", 9),
            release("v0.135.0-beta.1", 8, draft=True),
            release("v3.8.0-beta.1", 8),
        ]]
        native_schema.check("v0.135.0-beta.1", 7, pages)

    def test_first_native_release_passes(self):
        native_schema.check("v0.131.0-beta.1", 7, [[release("v3.8.0", 5)]])

    def test_bad_input_fails_closed(self):
        for tag, schema, pages in (("v3.9.0", 7, [[]]), ("v0.135.0", 0, [[]]), ("v0.135.0", 7, {})):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                native_schema.check(tag, schema, pages)


if __name__ == "__main__":
    unittest.main()
