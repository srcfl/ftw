#!/usr/bin/env bash
# Invoked by changesets/action as the `publish` step. By this point
# the Version PR has been merged, so package.json carries the new
# version and CHANGELOG.md has the latest section. Our job:
#
#   1. Read the bumped version from package.json.
#   2. Build release notes (codename header + extracted changelog).
#   3. Create + push the v$VERSION tag.
#   4. Create the GitHub Release pointed at that tag.
#
# changesets/action keys "did we publish?" off whether stdout contains
# a line of the form `New tag: <something>` — so we emit that line
# verbatim. Without it, downstream jobs' `published == 'true'` gate
# never trips and binaries + docker never build.
set -euo pipefail

VERSION="$(node -p "require('./package.json').version")"
TAG="v${VERSION}"

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "Tag ${TAG} already exists — skipping publish." >&2
  exit 0
fi

# Build release notes with codename header. Writes to ./release-notes.md.
node scripts/apply-codename.cjs "${VERSION}" > release-notes.md
echo "--- release-notes.md ---" >&2
cat release-notes.md >&2
echo "--- end ---" >&2

# Tag the current commit (HEAD = the merged Version PR commit on master).
git tag -a "${TAG}" -m "Release ${TAG}"
git push origin "${TAG}"

# Create the GitHub release. --notes-file pulls in the codename-annotated
# notes. The release is non-draft + non-prerelease — downstream jobs
# (binaries, docker, etc.) upload assets to it via `gh release upload`.
gh release create "${TAG}" \
  --title "${TAG}" \
  --notes-file release-notes.md

# Signal to changesets/action that a publish occurred. The action
# parses this line to set `outputs.published`.
echo "New tag: ${TAG}"
