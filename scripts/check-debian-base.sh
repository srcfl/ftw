#!/usr/bin/env bash
# Reports whether the Debian suite pinned by the container images is still the
# current Debian stable.
#
# The images pin a codename (debian:trixie-slim) rather than a suite alias
# (debian:stable-slim) so a major-version jump can never arrive silently on a
# rebuild. The cost of pinning is that nothing notices when a new stable ships —
# this is what notices.
#
# Truth comes from Debian's own Release file for the `stable` suite, not from
# registry tag listings: tags are noisy, rate-limited and say nothing about
# which suite Debian considers stable.
#
# Exit codes, matching scripts/sync-bundled-drivers.sh --behind:
#   0  pinned suite is current stable
#   1  a newer stable exists, or the Dockerfiles disagree with each other
#   2  the check could not run (network, parse) — never reported as "fine"
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

RELEASE_URL="${DEBIAN_RELEASE_URL:-https://deb.debian.org/debian/dists/stable/Release}"

# Read the pin out of the Dockerfiles instead of hard-coding it here, so this
# check cannot drift away from what actually ships.
pinned_debian() { sed -n 's/^FROM debian:\([a-z][a-z]*\)-slim.*/\1/p' "$1" | head -1; }

core=$(pinned_debian Dockerfile)
updater=$(pinned_debian Dockerfile.updater)

for pair in "Dockerfile:$core" "Dockerfile.updater:$updater"; do
  if [ -z "${pair#*:}" ]; then
    echo "could not read a Debian suite from ${pair%%:*}" >&2
    exit 2
  fi
done

echo "pinned suite:"
printf '  %-22s %s\n' "Dockerfile" "$core" "Dockerfile.updater" "$updater"

if [ "$core" != "$updater" ]; then
  echo ""
  echo "The two images no longer agree on one Debian suite. Sharing a single"
  echo "base layer is the reason they were aligned, and that benefit is lost"
  echo "while they differ."
  exit 1
fi

release=$(curl -fsSL --max-time 20 "$RELEASE_URL" 2>/dev/null) || {
  echo "could not fetch $RELEASE_URL" >&2
  exit 2
}
stable=$(printf '%s\n' "$release" | sed -n 's/^Codename: *//p' | head -1)
version=$(printf '%s\n' "$release" | sed -n 's/^Version: *//p' | head -1)
if [ -z "$stable" ]; then
  echo "no Codename field in $RELEASE_URL" >&2
  exit 2
fi

echo ""
echo "debian stable: $stable${version:+ (}${version}${version:+)}"

if [ "$core" = "$stable" ]; then
  echo ""
  echo "The pinned suite is current."
  exit 0
fi

echo ""
echo "A newer Debian stable is available: $core -> $stable"

# Probe availability before moving both Core and updater together.
if command -v docker >/dev/null 2>&1; then
  echo ""
  echo "image readiness:"
  for image in "debian:${stable}-slim"; do
    if docker manifest inspect "$image" >/dev/null 2>&1; then
      printf '  %-32s available\n' "$image"
    else
      printf '  %-32s NOT PUBLISHED YET\n' "$image"
    fi
  done
fi

exit 1
