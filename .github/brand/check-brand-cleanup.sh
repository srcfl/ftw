#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE_SHA="${BRAND_BASE_SHA:-HEAD}"
BASE_ROOT="$(mktemp -d)"
BASELINE="$(mktemp)"
CURRENT="$(mktemp)"
NEW="$(mktemp)"
UNCLASSIFIED="$(mktemp)"
ALLOWLIST="${ROOT}/.github/brand/compatibility-allowlist.txt"
trap 'rm -rf "${BASE_ROOT}"; rm -f "${BASELINE}" "${CURRENT}" "${NEW}" "${UNCLASSIFIED}"' EXIT

if ! command -v rg >/dev/null 2>&1; then
  echo "brand cleanup check requires ripgrep (rg)" >&2
  exit 2
fi

# Standalone product-copy forms only. Identifiers embedded in URLs, Go module
# paths, image names, hostnames and stable f42w wire values are inventoried and
# migrated by their dedicated phases rather than hidden in this baseline.
PATTERN='(?i)(?<![[:alnum:]/._-])(?:forty[-_ ]?two[-_ ]?watts|42[-_ ]?w(?:atts)?)(?![[:alnum:]/._-])|MIT[- ]licensed|licensed under (?:the )?MIT License|org\.opencontainers\.image\.licenses=MIT'

RG_ARGS=(
  --pcre2
  --no-heading
  --no-line-number
  --with-filename
  --color never
  --glob '!.git/**'
  --glob '!.claude/worktrees/**'
  --glob '!.codex/worktrees/**'
  --glob '!vendor/**'
  --glob '!web/vendor/**'
  --glob '!bin/**'
  --glob '!release/**'
  --glob '!optimizer/.venv/**'
  --glob '!**/__pycache__/**'
  --glob '!CHANGELOG.md'
  --glob '!.changeset/**'
  --glob '!.github/brand/**'
  --glob '!package-lock.json'
  --glob '!deploy/local-e2e/tier2/package-lock.json'
)

scan_tree() {
  local tree_root="$1"
  local output="$2"
  local raw
  local status
  raw="$(mktemp)"

  set +e
  (
    cd "${tree_root}"
    rg "${RG_ARGS[@]}" "${PATTERN}" .
  ) >"${raw}"
  status=$?
  set -e

  if [ "${status}" -gt 1 ]; then
    echo "ripgrep failed while scanning ${tree_root}" >&2
    rm -f "${raw}"
    return "${status}"
  fi

  sed 's#^\./##' "${raw}" | LC_ALL=C sort >"${output}"
  rm -f "${raw}"
}

if [[ "${BASE_SHA}" =~ ^0+$ ]]; then
  BASE_SHA="HEAD^"
fi
if ! git -C "${ROOT}" cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo "brand comparison base is not available: ${BASE_SHA}" >&2
  echo "Fetch the base revision or set BRAND_BASE_SHA to an available commit." >&2
  exit 2
fi

git -C "${ROOT}" archive --format=tar "${BASE_SHA}" | tar -xf - -C "${BASE_ROOT}"

scan_tree "${BASE_ROOT}" "${BASELINE}"
scan_tree "${ROOT}" "${CURRENT}"

# Base-only lines are fine: deleting legacy copy is progress. Current-only
# lines are new active legacy copy and need either FTW wording or explicit
# inventory review.
comm -13 "${BASELINE}" "${CURRENT}" >"${NEW}"

while IFS= read -r line; do
  allowed=false
  while IFS= read -r pattern; do
    [[ -n "${pattern}" && "${pattern}" != \#* ]] || continue
    if [[ "${line}" =~ ${pattern} ]]; then
      allowed=true
      break
    fi
  done <"${ALLOWLIST}"
  if [ "${allowed}" = false ]; then
    printf '%s\n' "${line}" >>"${UNCLASSIFIED}"
  fi
done <"${NEW}"

if [ -s "${UNCLASSIFIED}" ]; then
  echo "New active legacy-brand copy is not classified:" >&2
  sed 's/^/  /' "${UNCLASSIFIED}" >&2
  echo >&2
  echo "Use FTW wording, or add a narrow compatibility exception in" >&2
  echo ".github/brand/compatibility-allowlist.txt." >&2
  exit 1
fi

# Operational skills are copied for both Codex and Claude. Unlike prose, stale
# commands here can be executed against a real host, so do not grandfather
# removed command paths and keep the two tool surfaces byte-identical.
if ! diff -ru "${ROOT}/.agents/skills" "${ROOT}/.claude/skills" >/dev/null; then
  echo "operational skill copies under .agents/skills and .claude/skills differ" >&2
  diff -ru "${ROOT}/.agents/skills" "${ROOT}/.claude/skills" >&2 || true
  exit 1
fi

if rg --no-heading --line-number '(?:\./cmd/forty-two-watts|go/cmd/forty-two-watts)' \
  "${ROOT}/.agents/skills" "${ROOT}/.claude/skills"; then
  echo "operational skills reference the removed Go command path" >&2
  exit 1
fi

rg -q '\./cmd/ftw' "${ROOT}/.agents/skills/dev-backfill/SKILL.md" || {
  echo "dev-backfill skill is missing the canonical ./cmd/ftw build path" >&2
  exit 1
}
rg -q 'ghcr\.io/srcfl/ftw' "${ROOT}/.agents/skills/switching-ftw-deploy-mode/SKILL.md" || {
  echo "deploy-mode skill is missing the canonical Sourceful image" >&2
  exit 1
}

echo "brand cleanup check passed: no unclassified active legacy-product copy"
