#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'MESSAGE'
The macOS Docker installer is retired. This script makes no changes.
No native 0.x package for macOS has been published. Existing macOS FTW sites
stay on their current version until a tested migration path is available.
See https://github.com/srcfl/ftw/blob/master/docs/self-update.md
MESSAGE
exit 2
