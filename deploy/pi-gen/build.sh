#!/usr/bin/env bash
# Build the forty-two-watts Raspberry Pi OS image.
#
# Usage:
#   deploy/pi-gen/build.sh
#
# Runs on any Linux host with Docker — pi-gen shells out to Docker to
# run its build stages in a controlled chroot. macOS works too, but
# you'll need Docker Desktop with a recent enough engine (24+).
#
# Output lands in deploy/pi-gen/pi-gen/deploy/<IMG_NAME>-<date>.img.xz.
# That file is what the CI release job uploads to GitHub Releases.
#
# Env overrides:
#   PI_GEN_REF      pi-gen ref to check out (default: master). Pin to a
#                   tag in CI so image reproducibility doesn't depend
#                   on upstream HEAD moving.
#   FTW_COMPOSE     Override the docker-compose.yml source path (default:
#                   repo root). Useful for testing a compose variant.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PI_GEN_DIR="${SCRIPT_DIR}/pi-gen"
# 64-bit Raspberry Pi OS images build from pi-gen's `arm64` branch
# (per pi-gen's README: "64 bit images should be built from the arm64
# branch"); `master` defaults to armhf. The arm64 branch tracks trixie
# now (RELEASE=trixie, ENABLE_CLOUD_INIT=1, stage2/04-cloud-init present).
#
# Pin to a specific commit rather than the moving branch tip: early
# trixie images shipped cloud-init re-run bugs, so reproducibility +
# a deliberate bump matter here. This is the arm64 branch HEAD captured
# 2026-06-22; re-pin after validating a build on real hardware.
PI_GEN_REF="${PI_GEN_REF:-ca8aeed0ae300c2a89f55ce9617d5f96a27e99e5}"

# Sync repo-owned files into the stage's files/ directory. We copy
# rather than symlink because pi-gen's stage runner treats files/ as
# a plain tree and doesn't resolve symlinks pointing outside it.
# Both copies are gitignored; the canonical versions live at the
# repo root.
FILES_DIR="${SCRIPT_DIR}/stage-42w/01-ftw-setup/files"
FTW_COMPOSE="${FTW_COMPOSE:-${REPO_ROOT}/docker-compose.yml}"

install -m 0644 "${FTW_COMPOSE}"                                "${FILES_DIR}/docker-compose.yml"
install -m 0644 "${REPO_ROOT}/mosquitto/config/mosquitto.conf"  "${FILES_DIR}/mosquitto.conf"

if [ ! -d "${PI_GEN_DIR}" ]; then
    # `git clone --branch` only accepts branch/tag names, not arbitrary
    # commit SHAs, and we pin PI_GEN_REF to a SHA for reproducibility — so
    # clone the default branch; the explicit checkout below pins the ref
    # (works for SHA, branch, or tag). pi-gen is a small scripts repo, so a
    # full clone is cheap.
    git clone https://github.com/RPi-Distro/pi-gen.git "${PI_GEN_DIR}"
fi

# Pin to PI_GEN_REF on EVERY run, not just a fresh clone. An existing
# deploy/pi-gen/pi-gen would otherwise stay on whatever ref it was first
# cloned at and silently ignore a bumped PI_GEN_REF — a reproducibility hole
# that bites local rebuilds across a re-pin (CI is unaffected: it always
# starts from a clean checkout). Try a local checkout first; fetch only if
# the pinned ref isn't in the local clone yet. `-f` resets pi-gen's own
# tracked files but leaves our untracked stage-42w/config/SKIP additions
# (re-copied below) intact.
git -C "${PI_GEN_DIR}" checkout -f --quiet "${PI_GEN_REF}" 2>/dev/null || {
    git -C "${PI_GEN_DIR}" fetch --quiet origin
    git -C "${PI_GEN_DIR}" checkout -f --quiet "${PI_GEN_REF}"
}

# Copy (not symlink) our stage + config into the pi-gen checkout.
# build-docker.sh builds an image with `COPY . /pi-gen/` where . is
# the pi-gen directory — a symlink pointing OUTSIDE the build
# context becomes a dangling symlink inside the container, and
# pi-gen's `realpath /pi-gen/stage-42w` then fails with "No such
# file or directory" before a single stage runs.
rm -rf "${PI_GEN_DIR}/stage-42w"
cp -R "${SCRIPT_DIR}/stage-42w" "${PI_GEN_DIR}/stage-42w"
cp    "${SCRIPT_DIR}/config"    "${PI_GEN_DIR}/config"

# pi-gen honours SKIP files per stage: SKIP prevents the stage from
# running, SKIP_IMAGES prevents that stage from exporting an .img.
#
# stage2 RUNS now (on trixie it installs cloud-init + netplan and sets
# the systemd.run= first-boot/resize cmdline that replaced the old
# init=…firstboot path) — but we suppress its OWN image export so we
# don't waste a full export on the plain Lite image; stage-42w (last in
# STAGE_LIST, carries EXPORT_IMAGE) produces the only image.
touch "${PI_GEN_DIR}/stage2/SKIP_IMAGES" 2>/dev/null || true
# stage3-5 (desktop + NOOBS) are skipped entirely.
for stage in stage3 stage4 stage5; do
    touch "${PI_GEN_DIR}/${stage}/SKIP" \
          "${PI_GEN_DIR}/${stage}/SKIP_IMAGES" 2>/dev/null || true
done

cd "${PI_GEN_DIR}"

# build-docker.sh runs pi-gen inside a privileged Docker container so
# the build doesn't touch the host rootfs and works identically on
# Linux + macOS + GitHub Actions runners.
./build-docker.sh

echo ""
echo "Built image(s):"
ls -lah "${PI_GEN_DIR}/deploy/"*.img.* 2>/dev/null \
    || echo "  (no image files produced — check pi-gen logs above)"
