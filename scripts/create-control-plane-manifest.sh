#!/usr/bin/env bash
set -euo pipefail

tag="${1:?usage: create-control-plane-manifest.sh TAG REVISION OUTPUT}"
revision="${2:?usage: create-control-plane-manifest.sh TAG REVISION OUTPUT}"
output="${3:?usage: create-control-plane-manifest.sh TAG REVISION OUTPUT}"

[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$ ]] || {
  echo "invalid control-plane release tag: ${tag}" >&2
  exit 1
}
[[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid control-plane source revision: ${revision}" >&2
  exit 1
}

core_image="ghcr.io/srcfl/ftw:${tag}"
updater_image="ghcr.io/srcfl/ftw-updater:${tag}"
core_digest="$(scripts/inspect-image-digest.sh "${core_image}")"
updater_digest="$(scripts/inspect-image-digest.sh "${updater_image}")"

for digest in "${core_digest}" "${updater_digest}"; do
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "invalid immutable image digest: ${digest}" >&2
    exit 1
  }
done

jq -n \
  --arg release "${tag}" \
  --arg revision "${revision}" \
  --arg core_image "${core_image}" \
  --arg core_digest "${core_digest}" \
  --arg updater_image "${updater_image}" \
  --arg updater_digest "${updater_digest}" \
  '{
    schema_version: 1,
    release: $release,
    revision: $revision,
    components: {
      core: {image: $core_image, digest: $core_digest},
      updater: {image: $updater_image, digest: $updater_digest}
    }
  }' >"${output}"
