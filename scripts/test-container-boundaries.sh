#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

if grep -Eq 'COPY optimizer/|--from=optimizer|/opt/venv|FTW_OPTIMIZER_(PYTHON|DIR)' Dockerfile; then
  echo "Dockerfile must contain only core, drivers and web assets; use Dockerfile.optimizer for Python/CVXPY" >&2
  exit 1
fi

# The core runtime must stay a small, Python-free userland. This used to be
# asserted as `^FROM alpine:`, which was a proxy for the real rule and gave no
# diagnostic when it failed — Python originally reached core precisely by way of
# the optimizer's base image, so the check existed to catch a base that drags an
# interpreter in, not to mandate one distro. Assert that directly instead.
if grep -Eq '^FROM .*(python|pypy)' Dockerfile; then
  echo "Dockerfile must not build on a Python base image; use Dockerfile.optimizer for Python/CVXPY" >&2
  exit 1
fi
# wget is contractual, not incidental: the HEALTHCHECK uses it, and
# ftw-updater docker-execs it inside this image to decide whether an update
# commits. An updater already deployed in the field will keep doing so, so the
# core image cannot stop shipping wget without breaking self-update on every
# host running an older sidecar.
if ! grep -Eq 'wget' Dockerfile; then
  echo "Dockerfile must provide wget: the HEALTHCHECK and ftw-updater's readiness probe both exec it" >&2
  exit 1
fi
grep -q '^COPY optimizer/' Dockerfile.optimizer
grep -q '/out/ftw-backup' Dockerfile
grep -q '/app/ftw-backup' Dockerfile
grep -q -- '--chown=100:101 /out/ftw' Dockerfile
if grep -q 'chown -R 100:101 /app' Dockerfile; then
  echo "Dockerfile must set ownership while copying; a full-tree chown duplicates every app layer" >&2
  exit 1
fi
grep -q '^  ftw-optimizer:' docker-compose.yml
grep -q 'FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock' docker-compose.yml

echo "container module boundaries verified"
