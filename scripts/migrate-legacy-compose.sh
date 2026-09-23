#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'MESSAGE'
The Docker-to-Docker migration is retired. This script makes no changes.
Leave an existing FTW site on its current version. The guided native 0.x
migration will handle 1.x, 2.x and 3.x after it has been tested.
See https://github.com/srcfl/ftw/blob/master/docs/self-update.md
MESSAGE
exit 2
