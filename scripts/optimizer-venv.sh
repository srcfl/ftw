#!/usr/bin/env bash
# Build optimizer/.venv and install the optimizer into it.
#
# This is a script rather than two lines in the Makefile because choosing the
# interpreter is the whole job. The optimizer needs Python 3.11 or newer and a
# pip that can do a PEP 660 editable install. The python3 macOS ships is 3.9
# with pip 21.2 and fails on both counts, and the error it prints -- "File
# setup.py or setup.cfg not found" -- reads like a packaging fault in this
# repository. It is not one. It is the wrong interpreter, and finding that out
# has cost several people an afternoon each.
#
# Order of preference:
#   1. $PYTHON -- the interpreter the old recipe used, so a box where that
#      already worked keeps building exactly the venv it built before.
#   2. A python3.N on PATH, starting with the version the container image and
#      CI use, so a local venv resolves the same wheels they do.
#   3. uv, which can fetch an interpreter when the machine has none. Optional
#      throughout: it is used when nothing else works, never required.
# Nothing usable is an error that names both ways out.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT="${ROOT}/optimizer"
VENV="${PROJECT}/.venv"

# The floor comes from the package itself, so this cannot drift away from it.
FLOOR="$(sed -n 's/^requires-python[[:space:]]*=[[:space:]]*">=\([0-9][0-9.]*\)".*/\1/p' \
  "${PROJECT}/pyproject.toml" | head -1)"
FLOOR="${FLOOR:-3.11}"
FLOOR_MAJOR="${FLOOR%%.*}"
FLOOR_MINOR="${FLOOR#*.}"

# The version Dockerfile.optimizer and the CI optimizer job run. Preferred so a
# developer resolves the same wheels production does, but not required: any
# interpreter at or above the floor is accepted.
PREFERRED="3.12"

satisfies_floor() {
  local py="$1"
  command -v "$py" >/dev/null 2>&1 || return 1
  "$py" -c "import sys; raise SystemExit(0 if sys.version_info[:2] >= (${FLOOR_MAJOR}, ${FLOOR_MINOR}) else 1)" \
    >/dev/null 2>&1
}

# A pip older than 21.3 has no PEP 660 support and fails the same way 3.9 does.
# An interpreter new enough for the floor normally ships a new enough pip; this
# is here so the promise holds on the one that does not.
pip_understands_editable() {
  "${VENV}/bin/python" - <<'PY' >/dev/null 2>&1
import sys
try:
    from pip import __version__ as v
except Exception:
    raise SystemExit(1)
major, minor = (int(part) for part in v.split(".")[:2])
raise SystemExit(0 if (major, minor) >= (21, 3) else 1)
PY
}

# A half-built venv is the normal state here, not an edge case: the failure
# this script exists to prevent leaves one behind, built on the interpreter
# that could not do the install. Keep the environment when its own interpreter
# qualifies -- reinstalling into it is what an unchanged machine wants -- and
# otherwise throw it away and choose again.
if [ -x "${VENV}/bin/python" ] && satisfies_floor "${VENV}/bin/python"; then
  echo "optimizer: reusing .venv ($("${VENV}/bin/python" --version 2>&1))"
else
  if [ -e "${VENV}" ]; then
    echo "optimizer: replacing .venv, it has no python ${FLOOR}+"
    rm -rf "${VENV}"
  fi

  CHOSEN=""
  for candidate in "${PYTHON:-}" "python${PREFERRED}" python3.13 python3.14 python3.11 python3; do
    [ -n "${candidate}" ] || continue
    if satisfies_floor "${candidate}"; then
      CHOSEN="${candidate}"
      break
    fi
  done

  if [ -n "${CHOSEN}" ]; then
    echo "optimizer: building .venv with ${CHOSEN} ($("${CHOSEN}" --version 2>&1))"
    "${CHOSEN}" -m venv "${VENV}"
  elif command -v uv >/dev/null 2>&1; then
    # uv only fetches the interpreter here. --seed puts pip in the result, so
    # a venv built this way is indistinguishable from one built above and
    # nothing downstream has to know which route it came by.
    echo "optimizer: no python ${FLOOR}+ on PATH; building .venv with uv (python ${PREFERRED})"
    uv venv --seed --python "${PREFERRED}" "${VENV}"
  else
    cat >&2 <<MSG
The optimizer needs Python ${FLOOR} or newer and there is none on PATH.

  python3 is $(python3 -c 'import platform; print(platform.python_version())' 2>/dev/null || echo "not installed")

Either install an interpreter:

  brew install python@${PREFERRED}          # macOS
  apt install python${PREFERRED}-venv       # Debian/Ubuntu

or install uv, which fetches one itself:

  curl -LsSf https://astral.sh/uv/install.sh | sh

then run 'make optimizer-install' again. Core alone does not need this: the
optimizer is optional and 'cd go && go test ./...' runs without it.
MSG
    exit 1
  fi
fi

pip_understands_editable || "${VENV}/bin/python" -m pip install --quiet --upgrade pip
"${VENV}/bin/python" -m pip install -e "${PROJECT}[test]"
