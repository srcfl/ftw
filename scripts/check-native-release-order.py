#!/usr/bin/env python3
"""Refuse a native release older than a published native candidate."""

import json
from pathlib import Path
import re
import sys


TAG = re.compile(r"^v0\.(\d+)\.(\d+)(?:-beta\.(\d+))?$")


def rank(tag):
    match = TAG.fullmatch(tag)
    if match is None:
        return None
    minor, patch, beta = match.groups()
    return (int(minor), int(patch), beta is None, int(beta or 0))


def check(tag, pages):
    target = rank(tag)
    if target is None or target[:2] < (131, 0):
        raise ValueError(f"{tag} is not on the native release line (v0.131.0 or later)")
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("release list is not a list of pages")
    for page in pages:
        for release in page:
            if not isinstance(release, dict) or not isinstance(release.get("tag_name"), str):
                raise ValueError("release list has an invalid entry")
            if release.get("draft"):
                continue
            other_tag = release["tag_name"]
            other = rank(other_tag)
            if other is not None and other > target:
                raise ValueError(f"{tag} is older than published {other_tag}")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: check-native-release-order.py TAG RELEASE_PAGES_JSON")
    try:
        check(sys.argv[1], json.loads(Path(sys.argv[2]).read_text()))
    except (OSError, json.JSONDecodeError, ValueError) as error:
        raise SystemExit(str(error)) from error
