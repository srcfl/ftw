"""Write release notes from the candidate's changeset changelog."""

import argparse
import json
from pathlib import Path
import re


TAG = re.compile(r"v(0\.\d+\.\d+)(-beta\.\d+)?")
VERSION_HEADING = re.compile(r"^## \[?(\d+\.\d+\.\d+)")


def published_versions(pages):
    """Return the 0.x versions that already have a published release."""
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("release list is not a list of pages")
    versions = set()
    for page in pages:
        for release in page:
            if not isinstance(release, dict) or not isinstance(release.get("tag_name"), str):
                raise ValueError("release list has an invalid entry")
            match = TAG.fullmatch(release["tag_name"])
            if match and not release.get("draft"):
                versions.add(match.group(1))
    return versions


def section_end(lines, start):
    return next((i for i in range(start, len(lines))
                 if re.match(r"^## \[?\d", lines[i])), len(lines))


def section_changes(lines, start, end):
    changes = "\n".join(lines[start:end]).strip()
    changes = re.sub(r"<!--.*?-->", "", changes, flags=re.DOTALL).strip()
    if not re.sub(r"(?m)^#+.*$", "", changes).strip():
        return ""
    return re.sub(r"(?m)^- [0-9a-f]{7,40}: ", "- ", changes)


def unreleased_below(lines, start, published):
    """Sections after start up to the newest published version.

    A version that never got a release of its own reaches users in the next
    beta, so that beta lists its changes too. Without a published version
    below, the base is unknown and nothing is added.
    """
    found = []
    i = start
    while i < len(lines):
        match = VERSION_HEADING.match(lines[i])
        if not match:
            i += 1
            continue
        if match.group(1) in published:
            return found
        end = section_end(lines, i + 1)
        changes = section_changes(lines, i + 1, end)
        if changes:
            found.append((match.group(1), changes))
        i = end
    return []


def build_notes(receipt, changelog, repo="srcfl/ftw", published=None):
    tag = receipt["tag"]
    match = TAG.fullmatch(tag)
    if not match:
        raise ValueError(f"invalid native release tag: {tag}")
    version, beta = match.groups()
    lines = changelog.splitlines()
    try:
        start = lines.index(f"## {version}") + 1
    except ValueError:
        raise ValueError(f"CHANGELOG.md has no entry for {version}") from None
    end = section_end(lines, start)
    changes = section_changes(lines, start, end)
    if not changes:
        raise ValueError(f"CHANGELOG.md has no changes for {version}")
    heading = "Changes in this beta" if beta else "Changes in this release"
    notes = f"## {heading}\n\n{changes}\n\n"
    if beta and published and version not in published:
        for older, older_changes in unreleased_below(lines, end, published):
            notes += f"## From {older}, which had no release of its own\n\n{older_changes}\n\n"
    source_beta = receipt.get("source_beta")
    if source_beta:
        notes += f"Promoted from site-tested [{source_beta}](https://github.com/{repo}/releases/tag/{source_beta}).\n\n"
    notes += (f"[Full changelog](https://github.com/{repo}/blob/{tag}/CHANGELOG.md)"
              f" · [Source commit](https://github.com/{repo}/commit/{receipt['commit']})\n\n"
              f"<!-- ftw-state-schema-v2:{receipt['state_schema']} -->\n")
    return notes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("receipt", type=Path)
    parser.add_argument("changelog", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--repo", default="srcfl/ftw")
    parser.add_argument("--published", type=Path,
                        help="release pages JSON; a beta then also lists unreleased older versions")
    args = parser.parse_args()
    published = None
    if args.published:
        published = published_versions(json.loads(args.published.read_text()))
    notes = build_notes(json.loads(args.receipt.read_text()), args.changelog.read_text(),
                        args.repo, published)
    args.output.write_text(notes)


if __name__ == "__main__":
    main()
