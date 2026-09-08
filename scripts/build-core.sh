#!/usr/bin/env bash
# Build Core and its offline backup tool with DuckDB's bundled static libraries.
# Windows uses DuckDB's MinGW GCC 14.2.0 toolchain, supplied as CC/CXX.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
target_os=${1:?usage: build-core.sh OS ARCH OUTPUT_DIR}
target_arch=${2:?usage: build-core.sh OS ARCH OUTPUT_DIR}
mkdir -p "${3:?usage: build-core.sh OS ARCH OUTPUT_DIR}"
output=$(cd "$3" && pwd)
host_os=$(go env GOHOSTOS)

case "${target_os}/${target_arch}" in
  linux/amd64|linux/arm64|windows/amd64|darwin/amd64|darwin/arm64) ;;
  *) echo "No bundled DuckDB library for ${target_os}/${target_arch}" >&2; exit 1 ;;
esac

# Use the same Linux toolchain as the image when cross compilers are absent,
# or when a release explicitly asks for it. Docker exports only the binaries.
use_docker=${FTW_BUILD_DOCKER:-0}
if [[ "$target_os" == linux && -z "${CC:-}" ]]; then
  case "$target_arch" in
    amd64) compiler=x86_64-linux-gnu-gcc; cxx=x86_64-linux-gnu-g++ ;;
    arm64) compiler=aarch64-linux-gnu-gcc; cxx=aarch64-linux-gnu-g++ ;;
  esac
  if [[ "$host_os" != linux ]] || ! command -v "$compiler" >/dev/null 2>&1; then
    use_docker=1
  else
    export CC=$compiler
    export CXX=${CXX:-$cxx}
  fi
fi
if [[ "$use_docker" == 1 ]]; then
  [[ "$target_os" == linux ]] || { echo "The Docker builder supports Linux; use UCRT64 for Windows." >&2; exit 1; }
  exec docker buildx build --file "$root/Dockerfile" --target binaries \
    --platform "linux/$target_arch" \
    --build-arg "VERSION=${VERSION:-dev}" \
    --build-arg "CANDIDATE_TAG=${CANDIDATE_TAG:-}" \
    --build-arg "BUILD_ALL=${FTW_BUILD_ALL:-0}" \
    --output "type=local,dest=$output" "$root"
fi

if [[ "$target_os" == windows ]]; then
  if [[ "$host_os" != windows && -z "${CC:-}" ]]; then
    echo "Windows builds need DuckDB's MinGW GCC 14.2.0 toolchain. Set CC/CXX to its compilers." >&2
    exit 1
  fi
  export CC=${CC:-gcc}
  export CXX=${CXX:-g++}
  # The upstream libraries use UCRT's C++ ABI. An MSVCRT compiler can appear
  # to work until the final link, or produce a binary with mixed runtimes.
  if ! printf '#include <_mingw.h>\n#ifndef _UCRT\n#error UCRT64 required\n#endif\n' | "$CC" -E -x c - >/dev/null; then
    echo "DuckDB's Windows libraries require a UCRT-compatible GCC." >&2
    exit 1
  fi
fi

if [[ "$target_os" == darwin && "$host_os" != darwin ]]; then
  echo "macOS builds need a macOS SDK and compiler." >&2
  exit 1
fi

export GOOS=$target_os GOARCH=$target_arch CGO_ENABLED=1
ldflags="-s -w -X main.Version=${VERSION:-dev} -X main.CandidateTag=${CANDIDATE_TAG:-}"
if [[ "$target_os" == windows ]]; then
  # The Windows package must run without the compiler's runtime DLLs.
  ldflags+=" -linkmode external -extldflags '-static-libstdc++ -static-libgcc'"
fi
go version
"${CC:-cc}" --version
cd "$root/go"
if [[ "${FTW_BUILD_ALL:-0}" == 1 ]]; then
  set -- ./...
else
  set -- ./cmd/ftw ./cmd/ftw-backup
fi
# Preserve Go DNS and user lookup after enabling CGO for DuckDB.
go build -trimpath -tags=netgo,osusergo -ldflags "$ldflags" -o "$output/" "$@"
