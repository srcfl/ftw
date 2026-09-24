#!/usr/bin/env python3
"""Refuse a native release that changes the state schema.

A native box cannot yet take a schema step: its launcher refuses to stage
one, and the backup-and-restore path around it does not exist (ADR 0007,
decision 13). Publishing such a release would leave every native box
unable to update, so the release stops here until that path ships.
"""

import json
from pathlib import Path
import re
import sys


TAG = re.compile(r"^v0\.(\d+)\.(\d+)(?:-beta\.(\d+))?$")
MARKER = re.compile(r"<!-- ftw-state-schema-v2:\s*(\d+)\s*-->")


def rank(tag):
    match = TAG.fullmatch(tag)
    if match is None:
        return None
    minor, patch, beta = match.groups()
    return (int(minor), int(patch), beta is None, int(beta or 0))


def check(tag, schema, pages):
    target = rank(tag)
    if target is None:
        raise ValueError(f"{tag} is not a native release tag")
    if not isinstance(schema, int) or schema <= 0:
        raise ValueError(f"invalid state schema {schema!r}")
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("release list is not a list of pages")
    previous = None
    for page in pages:
        for release in page:
            if not isinstance(release, dict) or not isinstance(release.get("tag_name"), str):
                raise ValueError("release list has an invalid entry")
            other = rank(release["tag_name"])
            if release.get("draft") or other is None or other >= target:
                continue
            marker = MARKER.search(release.get("body") or "")
            if marker and (previous is None or other > previous[0]):
                previous = (other, release["tag_name"], int(marker.group(1)))
    if previous is not None and previous[2] != schema:
        raise ValueError(
            f"{tag} changes the state schema from {previous[2]} ({previous[1]}) to {schema}; "
            "native boxes cannot take that step until its backup and restore path ships (ADR 0007, decision 13)")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("usage: check-native-schema.py TAG STATE_SCHEMA RELEASE_PAGES_JSON")
    try:
        check(sys.argv[1], int(sys.argv[2]), json.loads(Path(sys.argv[3]).read_text()))
    except (OSError, json.JSONDecodeError, ValueError) as error:
        raise SystemExit(str(error)) from error
