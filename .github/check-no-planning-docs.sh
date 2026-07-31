#!/usr/bin/env bash
# Fails when a change adds agent planning artefacts to the repository.
#
# Plans, design specs and task breakdowns describe how work was going to be
# done. They are stale the moment the work lands, nobody reads them again,
# and they bury the docs that operators actually need. One PR carried 2,200
# lines of them; the author's own point was that this would never have
# happened if the repo had simply said so. So the repo says so, here.
#
# Put the reasoning in the PR description instead. It gets read while the
# change is being reviewed, and then it is archived rather than maintained.
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
Plans, specs and design notes do not belong in the repository — see the
"Working alongside other people" section in AGENTS.md. Move the reasoning
into the pull request description and drop the files.

Documentation that explains architecture, a safety invariant, or a step an
operator has to take is welcome under docs/ and is not what this checks for.
MSG
  exit 1
fi

echo "no agent planning documents added"
