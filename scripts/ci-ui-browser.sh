#!/usr/bin/env bash
# Browser smoke test for a running ftw instance.
#
# Usage:
#   scripts/ci-ui-browser.sh [base-url]
#
# BASE URL defaults to FTW_BASE_URL, then http://localhost:8080.
# Uses a locally installed Chrome/Chromium/Edge binary in headless mode and
# writes DOM dumps, screenshots, and API snapshots under artifacts/.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BASE_URL=${1:-${FTW_BASE_URL:-http://localhost:8080}}
BASE_URL=${BASE_URL%/}
ARTIFACT_DIR=${FTW_CI_ARTIFACT_DIR:-"$ROOT/artifacts/ci-ui/$(date -u +%Y%m%dT%H%M%SZ)"}
VIRTUAL_TIME_BUDGET_MS=${FTW_UI_VIRTUAL_TIME_BUDGET_MS:-8000}
BROWSER_TIMEOUT_S=${FTW_UI_BROWSER_TIMEOUT_S:-25}

mkdir -p "$ARTIFACT_DIR"

log() {
  printf '[ci-ui] %s\n' "$*"
}

fail() {
  printf '[ci-ui] ERROR: %s\n' "$*" >&2
  exit 1
}

find_browser() {
  if [ -n "${BROWSER_BIN:-}" ]; then
    if [ -x "$BROWSER_BIN" ]; then
      printf '%s\n' "$BROWSER_BIN"
      return 0
    fi
    fail "BROWSER_BIN is set but not executable: $BROWSER_BIN"
  fi

  local name
  for name in google-chrome-stable google-chrome chromium chromium-browser chrome microsoft-edge; do
    if command -v "$name" >/dev/null 2>&1; then
      command -v "$name"
      return 0
    fi
  done

  local path
  for path in \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/Applications/Chromium.app/Contents/MacOS/Chromium" \
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"; do
    if [ -x "$path" ]; then
      printf '%s\n' "$path"
      return 0
    fi
  done

  fail "no Chrome/Chromium browser found; set BROWSER_BIN=/path/to/browser"
}

curl_json() {
  local path=$1
  local slug=${path#/}
  slug=${slug//\//_}
  local out="$ARTIFACT_DIR/api-${slug}.json"
  local status

  log "GET $path"
  if ! status=$(curl -sS -o "$out" -w "%{http_code}" "$BASE_URL$path"); then
    fail "curl failed for $BASE_URL$path"
  fi
  case "$status" in
    2*|3*) ;;
    *) fail "$path returned HTTP $status (saved $out)" ;;
  esac
}

run_browser_once() {
  local browser=$1
  local headless_flag=$2
  local url=$3
  local size=$4
  local html=$5
  local png=$6
  local log_file=$7
  local profile=$8

  local flags=(
    "$headless_flag"
    "--disable-gpu"
    "--hide-scrollbars"
    "--no-first-run"
    "--no-default-browser-check"
    "--no-sandbox"
    "--user-data-dir=$profile"
    "--virtual-time-budget=$VIRTUAL_TIME_BUDGET_MS"
    "--window-size=$size"
    "--screenshot=$png"
    "--dump-dom"
    "$url"
  )

  "$browser" "${flags[@]}" > "$html" 2> "$log_file" &
  local pid=$!
  local i

  for i in $(seq 1 "$BROWSER_TIMEOUT_S"); do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      if wait "$pid"; then
        return 0
      fi
      if [ -s "$html" ] && [ -s "$png" ]; then
        return 0
      fi
      return 1
    fi
    sleep 1
  done

  {
    printf '\n[ci-ui] browser timeout after %ss; terminating pid %s\n' "$BROWSER_TIMEOUT_S" "$pid"
  } >> "$log_file"
  kill "$pid" >/dev/null 2>&1 || true
  sleep 1
  kill -9 "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true

  if [ -s "$html" ] && [ -s "$png" ]; then
    return 0
  fi
  return 124
}

run_headless() {
  local browser=$1
  local url=$2
  local size=$3
  local html=$4
  local png=$5
  local log_file=$6
  local profile_base

  profile_base="$ARTIFACT_DIR/profile-$(basename "$html" .html)"
  rm -rf "$profile_base-new" "$profile_base-old"

  if run_browser_once "$browser" "--headless=new" "$url" "$size" "$html" "$png" "$log_file" "$profile_base-new"; then
    return 0
  fi

  rm -f "$html" "$png"
  run_browser_once "$browser" "--headless" "$url" "$size" "$html" "$png" "$log_file" "$profile_base-old"
}

smoke_page() {
  local browser=$1
  local path=$2
  local needle=$3
  local size=$4
  local label=$5
  local slug=${path#/}

  if [ -z "$slug" ]; then
    slug=index
  fi
  slug=${slug//\//_}
  slug="$slug-$label"

  local html="$ARTIFACT_DIR/$slug.html"
  local png="$ARTIFACT_DIR/$slug.png"
  local browser_log="$ARTIFACT_DIR/$slug.browser.log"

  log "browser $path ($size)"
  run_headless "$browser" "$BASE_URL$path" "$size" "$html" "$png" "$browser_log"

  [ -s "$html" ] || fail "empty DOM dump for $path (saved $html)"
  [ -s "$png" ] || fail "empty screenshot for $path (saved $png)"
  grep -qi '<body' "$html" || fail "DOM dump for $path has no <body> (saved $html)"
  grep -qi "$needle" "$html" || fail "DOM dump for $path did not contain '$needle' (saved $html)"
}

browser=$(find_browser)
log "base URL: $BASE_URL"
log "browser: $browser"
log "artifacts: $ARTIFACT_DIR"

curl_json /api/health
curl_json /api/status
curl_json /api/drivers
curl_json /api/config

smoke_page "$browser" / "view-live" "1440,1000" desktop
smoke_page "$browser" /legacy "view-live" "1440,1000" desktop
smoke_page "$browser" /setup "wizard" "1440,1000" desktop
smoke_page "$browser" / "view-live" "390,844" mobile

log "ok"
