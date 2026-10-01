#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'MESSAGE'
The Docker-to-Docker migration is retired. This script makes no changes.
2.x and 3.x receive no further updates. Switch to new 0.x with a separate
setup and data; guided migration of old settings and history is not ready.
Do not install 3.x beta or bypass this check with an old script.
See https://github.com/srcfl/ftw/blob/master/docs/native-beta.md
MESSAGE
exit 2
