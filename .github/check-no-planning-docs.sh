#!/usr/bin/env bash
# Fails when a change adds agent planning artefacts to the repository.
#
# Short Markdown proposals are welcome in the PR description or relevant
# maintained docs. This check rejects known scratch paths and dated agent
# work plans, not proposals as a contribution type. See CONTRIBUTING.md.
set -euo pipefail

BASE_SHA="${PLANNING_DOCS_BASE_SHA:-}"
if [ -z "${BASE_SHA}" ]; then
  echo "PLANNING_DOCS_BASE_SHA not set; nothing to compare against" >&2
  exit 0
fi

# Only files this change ADDS are considered — existing ones are somebody
# else's cleanup, not this PR's problem.
added="$(git diff --name-only --diff-filter=A "${BASE_SHA}"...HEAD || true)"
[ -z "${added}" ] && exit 0

offenders=""
while IFS= read -r file; do
  [ -z "${file}" ] && continue
  case "${file}" in
    # Directories whose whole purpose is agent scratch work.
    docs/superpowers/*|docs/plans/*|docs/specs/*|.claude/plans/*|plans/*|specs/*)
      offenders="${offenders}${file}"$'\n' ;;
    # Dated plan/spec/design notes anywhere under docs/.
    docs/*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*-plan*.md|\
    docs/*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*-spec*.md|\
    docs/*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*-design*.md)
      offenders="${offenders}${file}"$'\n' ;;
  esac
done <<< "${added}"

if [ -n "${offenders}" ]; then
  echo "This change adds agent planning documents:" >&2
  echo "${offenders}" >&2
  cat >&2 <<'MSG'
Task breakdowns and agent scratch notes do not belong in the repository.
Move work reasoning into the PR description and drop these files.

Short Markdown proposals are welcome in the PR description or relevant
maintained docs. Mark proposed behaviour as proposed. Architecture, safety
and operator docs remain welcome. See CONTRIBUTING.md and AGENTS.md.
MSG
  exit 1
fi

echo "no agent planning documents added"
