#!/usr/bin/env bash
# Core and backup are pure Go. The one-off beta converter has its own module.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
target_os=${1:?usage: build-core.sh OS ARCH OUTPUT_DIR}
target_arch=${2:?usage: build-core.sh OS ARCH OUTPUT_DIR}
mkdir -p "${3:?usage: build-core.sh OS ARCH OUTPUT_DIR}"
output=$(cd "$3" && pwd)
export GOOS=$target_os GOARCH=$target_arch CGO_ENABLED=0
cd "$root/go"
if [[ "${FTW_BUILD_ALL:-0}" == 1 ]]; then
  set -- ./...
else
  set -- ./cmd/ftw ./cmd/ftw-backup ./cmd/ftw-launcher
fi
go build -trimpath -tags=netgo,osusergo \
  -ldflags "-s -w -X main.Version=${VERSION:-dev} -X main.CandidateTag=${CANDIDATE_TAG:-}" \
  -o "$output/" "$@"
