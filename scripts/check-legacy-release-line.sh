#!/usr/bin/env bash
# The old Docker discovery endpoints are shared by already installed boxes.
# This checks historical tag compatibility for old recovery tools; it does not
# authorize publication. Current workflows block the retired release path.
set -euo pipefail

tag="${1:?release tag is required}"
if [[ "${tag}" =~ ^v2\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$ ]]; then
  exit 0
fi

echo "${tag} cannot use the legacy Docker release channel; keep GitHub latest and Docker aliases on 2.x." >&2
exit 1
