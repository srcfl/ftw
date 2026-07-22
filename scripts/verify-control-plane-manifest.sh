#!/usr/bin/env bash
set -euo pipefail

manifest="${1:?usage: verify-control-plane-manifest.sh MANIFEST TAG REVISION}"
tag="${2:?usage: verify-control-plane-manifest.sh MANIFEST TAG REVISION}"
revision="${3:?usage: verify-control-plane-manifest.sh MANIFEST TAG REVISION}"

jq -e \
  --arg release "${tag}" \
  --arg revision "${revision}" \
  --arg core "ghcr.io/srcfl/ftw:${tag}" \
  --arg updater "ghcr.io/srcfl/ftw-updater:${tag}" '
    .schema_version == 1
    and .release == $release
    and .revision == $revision
    and ((.components | keys | sort) == ["core", "updater"])
    and .components.core.image == $core
    and .components.updater.image == $updater
    and (.components.core.digest | test("^sha256:[0-9a-f]{64}$"))
    and (.components.updater.digest | test("^sha256:[0-9a-f]{64}$"))
  ' "${manifest}" >/dev/null

core_ref="$(jq -r '.components.core.image' "${manifest}")"
updater_ref="$(jq -r '.components.updater.image' "${manifest}")"
expected_core="$(jq -r '.components.core.digest' "${manifest}")"
expected_updater="$(jq -r '.components.updater.digest' "${manifest}")"
actual_core="$(scripts/inspect-image-digest.sh "${core_ref}")"
actual_updater="$(scripts/inspect-image-digest.sh "${updater_ref}")"

[ "${actual_core}" = "${expected_core}" ] || {
  echo "Core tag moved: ${actual_core} != ${expected_core}" >&2
  exit 1
}
[ "${actual_updater}" = "${expected_updater}" ] || {
  echo "updater tag moved: ${actual_updater} != ${expected_updater}" >&2
  exit 1
}
