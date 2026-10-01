#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'MESSAGE'
The macOS Docker installer is retired. This script makes no changes.
No native 0.x package for macOS has been published. 2.x and 3.x receive no
further updates. Use a new setup on 64-bit Linux to follow current releases.
Keep old data; guided migration is not ready.
See https://github.com/srcfl/ftw/blob/master/docs/native-beta.md
MESSAGE
exit 2
