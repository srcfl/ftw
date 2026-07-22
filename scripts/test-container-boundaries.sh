#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

if grep -Eq 'COPY optimizer/|--from=optimizer|/opt/venv|FTW_OPTIMIZER_(PYTHON|DIR)' Dockerfile; then
  echo "Dockerfile must contain only core, drivers and web assets; use Dockerfile.optimizer for Python/CVXPY" >&2
  exit 1
fi

grep -q '^FROM alpine:' Dockerfile
grep -q '^COPY optimizer/' Dockerfile.optimizer
grep -q '/out/ftw-backup' Dockerfile
grep -q '/app/ftw-backup' Dockerfile
grep -q 'HEALTHCHECK.*start-period=30m' Dockerfile
grep -q 'http://127.0.0.1:8080/api/status' Dockerfile
if grep -A1 '^HEALTHCHECK' Dockerfile | grep -q '/api/health'; then
  echo "Core container health must use readiness, not boot liveness" >&2
  exit 1
fi
grep -q '^  ftw-optimizer:' docker-compose.yml
grep -q 'FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock' docker-compose.yml

echo "container module boundaries verified"
