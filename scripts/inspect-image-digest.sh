#!/usr/bin/env bash

set -euo pipefail

ref="${1:?usage: inspect-image-digest.sh IMAGE_REF}"
attempts="${GHCR_INSPECT_ATTEMPTS:-6}"
base_delay_s="${GHCR_INSPECT_DELAY_S:-2}"

for ((attempt = 1; attempt <= attempts; attempt++)); do
  set +e
  output="$(docker buildx imagetools inspect "${ref}" 2>/dev/null)"
  status=$?
  set -e

  if [ "${status}" -eq 0 ]; then
    digest="$(printf '%s\n' "${output}" | awk '$1 == "Digest:" {print $2; exit}')"
    if [ -n "${digest}" ]; then
      printf '%s\n' "${digest}"
      exit 0
    fi
  fi

  if [ "${attempt}" -lt "${attempts}" ]; then
    delay_s=$((base_delay_s * attempt))
    echo "manifest not visible yet (${attempt}/${attempts}): ${ref}; retrying in ${delay_s}s" >&2
    sleep "${delay_s}"
  fi
done

echo "manifest did not become readable after ${attempts} attempts: ${ref}" >&2
exit 1
