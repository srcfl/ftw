#!/usr/bin/env bash
# The old Docker discovery endpoints are shared by already installed boxes.
# Only 2.x maintenance may publish through these workflows until a separate
# native 0.x release path keeps those endpoints on the old line.
set -euo pipefail

tag="${1:?release tag is required}"
if [[ "${tag}" =~ ^v2\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$ ]]; then
  exit 0
fi

echo "${tag} cannot use the legacy Docker release channel; keep GitHub latest and Docker aliases on 2.x." >&2
exit 1
