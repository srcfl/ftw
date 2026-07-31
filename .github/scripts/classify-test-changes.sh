#!/usr/bin/env bash
# Classify the paths changed by test.yml. Keep this beside the workflow so
# the routing rules have a small, deterministic contract test.

set -euo pipefail

core=false
optimizer=false
roofmodel=false
web=false
drivers=false
compose=false

while IFS= read -r file; do
  [ -n "${file}" ] || continue
  case "${file}" in
    # A pin move changes the released driver set even though it is JSON, not
    # Lua. It must therefore run both snapshot checks and the normal core
    # suite that validates the generated driver facts.
    drivers/*.lua|drivers/BUNDLED_SOURCE.json|scripts/sync-bundled-drivers.sh)
      core=true
      drivers=true
      ;;
    # contract/ is here because the Go suite checks the generated constants
    # against the registry.
    go/*|contract/*|drivers/*|config*.yaml|Dockerfile|Dockerfile.updater|.dockerignore)
      core=true
      ;;
    optimizer/*|Dockerfile.optimizer|go/internal/mpc/*|go/cmd/ftw/main.go)
      optimizer=true
      ;;
    # Python roof-geometry module only. Its Go host lives under go/, which
    # the core suite already covers.
    roofmodel/*)
      roofmodel=true
      ;;
    web/*|package.json|package-lock.json)
      web=true
      ;;
    Dockerfile|Dockerfile.optimizer|docker-compose*.yml|scripts/enable-modular-stack.sh|scripts/migrate-legacy-compose.sh|scripts/test-modular-compose.sh|scripts/test-container-boundaries.sh|scripts/install-macos.sh|.github/scripts/classify-test-changes.sh|.github/scripts/test-change-classifier.sh)
      compose=true
      ;;
    .github/workflows/beta.yml|.github/workflows/release.yml|.github/workflows/release-assets.yml|scripts/check-stable-release.py|scripts/check-ghcr-write-access.sh|scripts/test-ghcr-write-access.sh|scripts/test-exact-image-promotion.sh|scripts/github-release-by-id.sh|scripts/test-github-release-by-id.sh|scripts/promote-paired-latest.sh|scripts/test-promote-paired-latest.sh)
      compose=true
      ;;
    Makefile|.github/workflows/test.yml)
      core=true
      optimizer=true
      roofmodel=true
      web=true
      drivers=true
      compose=true
      ;;
  esac
  case "${file}" in
    optimizer/*|Dockerfile.optimizer|go/internal/mpc/*|go/cmd/ftw/main.go)
      optimizer=true
      ;;
  esac
done

printf 'core=%s\noptimizer=%s\nroofmodel=%s\nweb=%s\ndrivers=%s\ncompose=%s\n' \
  "${core}" "${optimizer}" "${roofmodel}" "${web}" "${drivers}" "${compose}"
