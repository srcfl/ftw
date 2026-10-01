#!/usr/bin/env bash
# A rebuild must not read or send the site's private data to Docker.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if ! docker info >/dev/null 2>&1; then
  echo 'SKIP release Docker context: Docker daemon unavailable'
  exit 0
fi
probe=$(mktemp -d)
cleanup() {
  chmod 600 "$probe/context/data/applink.json" 2>/dev/null || true
  rm -rf "$probe"
}
trap cleanup EXIT
mkdir -p "$probe/context/data"
printf 'private test fixture\n' > "$probe/context/data/applink.json"
printf 'private env fixture\n' > "$probe/context/.env"
printf 'old installer fixture\n' > "$probe/context/install.sh"
chmod 000 "$probe/context/data/applink.json"
cp "$root/deploy/docker/.dockerignore" "$probe/context/.dockerignore"
# COPY forces Docker to evaluate the context; no base image or network needed.
printf 'FROM scratch\nCOPY . /context/\n' > "$probe/context/Dockerfile"
docker buildx build --network=none --progress=plain --output "type=local,dest=$probe/result" "$probe/context"
test -d "$probe/result/context"
if find "$probe/result/context" -type f | grep -Eq '/(applink.json|\.env|install.sh)$'; then
  echo 'Private or unrelated files entered the release Docker context' >&2
  exit 1
fi
echo 'release Docker context excludes private data and unrelated files'
