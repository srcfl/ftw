#!/usr/bin/env bash
# install-ftw-connect.sh — download and install the ftw-connect binary
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install-ftw-connect.sh | bash
#
# Or with a custom install prefix for testing (no sudo required):
#   PREFIX=/tmp/install-test bash install-ftw-connect.sh
#
# What it does:
#   1. Detects your OS and CPU architecture.
#   2. Downloads the matching ftw-connect binary from the latest GitHub release.
#   3. Installs to /usr/local/bin (or $PREFIX/bin, or ~/.local/bin as fallback).
#   4. Verifies by running ftw-connect --version.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────

REPO="frahlg/forty-two-watts"
BINARY_NAME="ftw-connect"

# PREFIX env var overrides the install root (useful for smoke-testing
# without touching /usr/local/bin).  Falls back to /usr/local if unset.
INSTALL_ROOT="${PREFIX:-/usr/local}"

# ── OS / arch detection ───────────────────────────────────────────────────────

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Linux)  GOOS="linux"   ;;
  Darwin) GOOS="darwin"  ;;
  MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
  *)
    echo "ERROR: unsupported OS: $OS" >&2
    echo "       Supported: Linux, Darwin (macOS), Windows (Git Bash / MSYS2)" >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64"  ;;
  arm64|aarch64) GOARCH="arm64"  ;;
  armv7l)
    echo "ERROR: 32-bit ARM is not supported." >&2
    echo "       ftw-connect requires a 64-bit OS." >&2
    exit 1
    ;;
  *)
    echo "ERROR: unsupported architecture: $ARCH" >&2
    echo "       Supported: x86_64/amd64, arm64/aarch64" >&2
    exit 1
    ;;
esac

EXT=""
if [ "$GOOS" = "windows" ]; then
  EXT=".exe"
fi

ASSET_NAME="${BINARY_NAME}-${GOOS}-${GOARCH}${EXT}"

# ── Resolve latest release tag ────────────────────────────────────────────────

# Prefer gh CLI when available (avoids rate-limiting on unauthenticated API
# calls from shared CI environments).  Fall back to curl.
if command -v gh >/dev/null 2>&1; then
  TAG="$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null)" || TAG=""
fi

if [ -z "${TAG:-}" ]; then
  if command -v curl >/dev/null 2>&1; then
    DL_CMD="curl"
  elif command -v wget >/dev/null 2>&1; then
    DL_CMD="wget"
  else
    echo "ERROR: neither curl nor wget found on PATH." >&2
    echo "       Install one and try again." >&2
    exit 1
  fi

  # GitHub redirects /releases/latest to /releases/tag/<tag>; the Location
  # header carries the resolved tag.
  if [ "$DL_CMD" = "curl" ]; then
    TAG="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
      "https://github.com/${REPO}/releases/latest" \
      | sed 's|.*/tag/||')"
  else
    # wget doesn't print the redirect URL easily; use the API instead.
    TAG="$(wget -qO- \
      "https://api.github.com/repos/${REPO}/releases/latest" \
      | grep '"tag_name"' \
      | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  fi
fi

if [ -z "${TAG:-}" ]; then
  echo "ERROR: could not resolve the latest release tag from GitHub." >&2
  echo "       Check your internet connection or visit:" >&2
  echo "       https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET_NAME}"

# ── Install directory ─────────────────────────────────────────────────────────

# Honour PREFIX for testing.  Without PREFIX we pick /usr/local/bin when
# writable; otherwise fall back to ~/.local/bin (created if absent).
if [ -n "${PREFIX:-}" ]; then
  INSTALL_DIR="${INSTALL_ROOT}/bin"
else
  if [ -w "/usr/local/bin" ] || [ "$(id -u)" -eq 0 ]; then
    INSTALL_DIR="/usr/local/bin"
  elif command -v sudo >/dev/null 2>&1; then
    INSTALL_DIR="/usr/local/bin"
    USE_SUDO="sudo"
  else
    INSTALL_DIR="${HOME}/.local/bin"
  fi
fi

mkdir -p "$INSTALL_DIR"

DEST="${INSTALL_DIR}/${BINARY_NAME}${EXT}"

# ── Download ──────────────────────────────────────────────────────────────────

echo "Downloading ${ASSET_NAME} (${TAG}) ..."
echo "  from: ${DOWNLOAD_URL}"
echo "  to:   ${DEST}"

TMP_FILE="$(mktemp)"
# Ensure the temp file is cleaned up on exit regardless of success/failure.
trap 'rm -f "$TMP_FILE"' EXIT

if command -v curl >/dev/null 2>&1; then
  if ! curl -fsSL --retry 3 --retry-delay 2 -o "$TMP_FILE" "$DOWNLOAD_URL"; then
    echo "ERROR: download failed." >&2
    echo "       URL: ${DOWNLOAD_URL}" >&2
    echo "       Check that release ${TAG} has an asset named '${ASSET_NAME}'." >&2
    echo "       Available assets: https://github.com/${REPO}/releases/tag/${TAG}" >&2
    exit 1
  fi
elif command -v wget >/dev/null 2>&1; then
  if ! wget -qO "$TMP_FILE" "$DOWNLOAD_URL"; then
    echo "ERROR: download failed." >&2
    echo "       URL: ${DOWNLOAD_URL}" >&2
    exit 1
  fi
fi

chmod +x "$TMP_FILE"

# ── Install ───────────────────────────────────────────────────────────────────

if [ -n "${USE_SUDO:-}" ]; then
  sudo mv "$TMP_FILE" "$DEST"
  sudo chmod +x "$DEST"
else
  mv "$TMP_FILE" "$DEST"
fi

# Disable the EXIT trap now that we've moved the file.
trap - EXIT

# ── Verify ────────────────────────────────────────────────────────────────────

# Add INSTALL_DIR to PATH for this script's process if it isn't already there
# (needed when installing to ~/.local/bin on a fresh system).
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) export PATH="${INSTALL_DIR}:${PATH}" ;;
esac

echo ""
if "$DEST" --version 2>/dev/null; then
  echo ""
  echo "ftw-connect installed successfully to: ${DEST}"
  echo ""
  echo "If ${INSTALL_DIR} is not in your PATH, add it:"
  echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
else
  echo "WARNING: ${DEST} installed but '--version' failed." >&2
  echo "         The binary may not be compatible with this OS/arch." >&2
  echo "         Installed to: ${DEST}" >&2
  exit 1
fi
