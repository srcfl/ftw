#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLASSIFIER="${ROOT}/.github/scripts/classify-test-changes.sh"

assert_paths() {
  local paths="$1"
  shift
  local actual expected
  actual="$(printf '%s\n' "${paths}" | bash "${CLASSIFIER}")"
  expected="$(printf '%s\n' "$@")"
  if [ "${actual}" != "${expected}" ]; then
    echo "unexpected test-suite routing for: ${paths}" >&2
    diff -u <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}") >&2 || true
    exit 1
  fi
}

# This was the release hole: before the special case came first, this path
# matched drivers/* and selected only core.
assert_paths 'drivers/BUNDLED_SOURCE.json' \
  'core=true' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=true' 'compose=false'

assert_paths 'drivers/example.lua' \
  'core=true' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=true' 'compose=false'

assert_paths 'config.example.yaml' \
  'core=true' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=false'

assert_paths '.github/workflows/test.yml' \
  'core=true' 'optimizer=true' 'roofmodel=true' 'web=true' 'drivers=true' 'compose=true'

assert_paths 'roofmodel/ftw_roofmodel/pipeline.py' \
  'core=false' 'optimizer=false' 'roofmodel=true' 'web=false' 'drivers=false' 'compose=false'

assert_paths '.github/scripts/classify-test-changes.sh' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

assert_paths '.github/workflows/release-assets.yml' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

assert_paths 'scripts/promote-paired-latest.sh' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

assert_paths 'scripts/github-release-by-id.sh' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

assert_paths 'scripts/test-github-release-by-id.sh' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

assert_paths 'scripts/check-stable-release.py' \
  'core=false' 'optimizer=false' 'roofmodel=false' 'web=false' 'drivers=false' 'compose=true'

echo "test workflow path classifier contract passed"
