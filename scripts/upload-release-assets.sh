#!/usr/bin/env bash
# Resume an upload without replacing any asset that users could have fetched.
set -euo pipefail

tag=${1:?usage: upload-release-assets.sh TAG FILE...}
shift
if [ "$#" -eq 0 ]; then
  echo "at least one release asset is required" >&2
  exit 1
fi
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
existing=$(gh release view "$tag" --repo "$repo" --json assets)
download=$(mktemp -d)
trap 'rm -rf "$download"' EXIT
missing=()

for asset in "$@"; do
  test -f "$asset"
  name=$(basename "$asset")
  if jq -e --arg name "$name" '.assets | any(.name == $name)' <<<"$existing" >/dev/null; then
    gh release download "$tag" --repo "$repo" --pattern "$name" --dir "$download"
    if ! cmp -s "$asset" "$download/$name"; then
      echo "$tag/$name already exists with different bytes; refusing to replace it." >&2
      exit 1
    fi
    echo "Verified existing $tag/$name"
  else
    missing+=("$asset")
  fi
done

# Check all existing bytes before extending a partially uploaded release.
if [ "${#missing[@]}" -gt 0 ]; then
  for asset in "${missing[@]}"; do
    gh release upload "$tag" "$asset" --repo "$repo"
  done
fi
