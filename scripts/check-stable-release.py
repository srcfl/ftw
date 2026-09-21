#!/usr/bin/env python3
"""Fail closed when a stable release would downgrade or expose wrong assets."""

import json
import re
import sys
from pathlib import Path


STABLE_TAG = re.compile(r"^v([0-9]+)\.([0-9]+)\.([0-9]+)$")
REQUIRED_ASSETS = sorted(
    [
        "ftw-promotion-receipt.json",
        "os_list.json",
        "ftw-linux-amd64.tar.gz",
        "ftw-linux-amd64.tar.gz.sha256",
        "forty-two-watts-linux-amd64.tar.gz",
        "forty-two-watts-linux-amd64.tar.gz.sha256",
        "ftw-linux-arm64.tar.gz",
        "ftw-linux-arm64.tar.gz.sha256",
        "forty-two-watts-linux-arm64.tar.gz",
        "forty-two-watts-linux-arm64.tar.gz.sha256",
    ]
)


def version(tag: str) -> tuple[int, int, int]:
    match = STABLE_TAG.fullmatch(tag)
    if match is None:
        raise ValueError(f"invalid stable tag {tag!r}")
    parsed = tuple(int(part) for part in match.groups())
    if tag != f"v{parsed[0]}.{parsed[1]}.{parsed[2]}":
        raise ValueError(f"non-canonical stable tag {tag!r}")
    return parsed


def check_order(candidate: str, pages: object) -> None:
    candidate_version = version(candidate)
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("release inventory is not a paginated JSON list")
    public = []
    for release in (item for page in pages for item in page):
        if not isinstance(release, dict):
            raise ValueError("release inventory contains a non-object")
        tag = release.get("tag_name")
        draft = release.get("draft")
        prerelease = release.get("prerelease")
        published_at = release.get("published_at")
        if (
            not isinstance(tag, str)
            or type(draft) is not bool
            or type(prerelease) is not bool
        ):
            raise ValueError("release inventory has invalid tag or channel fields")
        if published_at is not None and not isinstance(published_at, str):
            raise ValueError("release inventory has invalid publication time")
        if draft is False and prerelease is False and STABLE_TAG.fullmatch(tag):
            if not published_at:
                raise ValueError(f"public stable {tag} has no publication time")
            public.append((version(tag), tag))
    if public:
        newest_version, newest_tag = max(public)
        if candidate_version < newest_version:
            raise ValueError(
                f"{candidate} is older than newest public stable {newest_tag}"
            )


def check_assets(tag: str, release: object) -> None:
    if not isinstance(release, dict):
        raise ValueError("release metadata is not an object")
    assets = release.get("assets")
    if (
        release.get("tagName") != tag
        or release.get("isDraft") is not True
        or release.get("isPrerelease") is not False
        or release.get("publishedAt") is not None
        or not isinstance(assets, list)
    ):
        raise ValueError(f"{tag} is not the expected draft release")
    if any(not isinstance(asset, dict) for asset in assets):
        raise ValueError("release assets contain a non-object")
    if len(assets) != len(REQUIRED_ASSETS) or any(
        not isinstance(asset.get("name"), str) for asset in assets
    ):
        raise ValueError(f"release must contain exactly {len(REQUIRED_ASSETS)} named assets")
    names = sorted(asset["name"] for asset in assets)
    if names != REQUIRED_ASSETS:
        raise ValueError(f"release asset names differ: got {names!r}")
    if any(
        asset.get("state") != "uploaded"
        or type(asset.get("size")) is not int
        or asset["size"] <= 0
        for asset in assets
    ):
        raise ValueError("every required release asset must be uploaded and non-empty")


def check_draft(
    tag: str, state_schema: str, expected_body_path: str, release: object
) -> None:
    if not re.fullmatch(r"[1-9][0-9]*", state_schema):
        raise ValueError("state schema must be a positive integer")
    with open(expected_body_path, encoding="utf-8") as body_file:
        expected_body = body_file.read()
    # Cores before v3.6.0-beta.1 read only the legacy marker and copy their
    # whole history when it differs from their own schema, 4 (#1302). It must
    # stay at state-schema.json's legacy_marker; newer Cores read the v2 one.
    legacy_prefix = "<!-- ftw-state-schema:"
    legacy_marker = f"<!-- ftw-state-schema:{legacy_marker_value()} -->"
    marker_prefix = "<!-- ftw-state-schema-v2:"
    marker = f"<!-- ftw-state-schema-v2:{state_schema} -->"
    if (
        expected_body.count(legacy_prefix) != 1
        or expected_body.count(legacy_marker) != 1
        or expected_body.count(marker_prefix) != 1
        or expected_body.count(marker) != 1
    ):
        raise ValueError(
            "release body must contain exactly the legacy floor marker and the expected v2 state-schema marker"
        )
    if not isinstance(release, dict) or (
        release.get("tagName") != tag
        or release.get("name") != tag
        or release.get("isDraft") is not True
        or release.get("isPrerelease") is not False
        or release.get("publishedAt") is not None
        or release.get("body") != expected_body
    ):
        raise ValueError(f"{tag} draft title, body or channel state is stale")


def legacy_marker_value() -> str:
    schema_path = Path(__file__).resolve().parent.parent / "state-schema.json"
    with open(schema_path, encoding="utf-8") as schema_file:
        value = json.load(schema_file).get("legacy_marker")
    if type(value) is not int or value <= 0:
        raise ValueError("state-schema.json must contain a positive integer legacy_marker")
    return str(value)


def main() -> None:
    if len(sys.argv) < 3 or sys.argv[1] not in {"order", "assets", "draft"}:
        raise SystemExit(
            f"usage: {sys.argv[0]} order|assets TAG | draft TAG SCHEMA BODY_FILE"
        )
    data = json.load(sys.stdin)
    if sys.argv[1] == "draft":
        if len(sys.argv) != 5:
            raise ValueError("draft check requires TAG, SCHEMA and BODY_FILE")
        check_draft(sys.argv[2], sys.argv[3], sys.argv[4], data)
    elif len(sys.argv) != 3:
        raise ValueError(f"{sys.argv[1]} check requires one TAG")
    elif sys.argv[1] == "order":
        check_order(sys.argv[2], data)
    else:
        check_assets(sys.argv[2], data)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError) as error:
        raise SystemExit(str(error)) from error
