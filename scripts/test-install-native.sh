#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
installer="${root}/scripts/install.sh"
data="${root}/scripts/testdata/install"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "$*" >&2
  exit 1
}

bash -n "$installer"
bash "$installer" --help > "$work/help"
grep -q 'Without --tag the installer takes the newest stable release' "$work/help"

# A power cut must not lose the pending record or the copied release: each is
# flushed before the step that depends on it.
line_of() { grep -nF -- "$1" "$installer" | head -1 | cut -d: -f1; }
synced_between() {
  awk -v a="$1" -v b="$2" 'NR > a && NR < b && $1 == "as_root" && $2 == "sync" { found = 1 } END { exit !found }' "$installer"
}
pending_line="$(line_of 'install -m 0600 "${work}/pending" "$pending"')"
copy_line="$(line_of 'as_root cp -a "${stage}/." /opt/ftw/')"
enable_line="$(line_of 'systemctl enable --now ftw.service')"
if ! synced_between "$pending_line" "$copy_line" || ! synced_between "$copy_line" "$enable_line"; then
  fail "installer starts FTW before its pending record and release are on disk"
fi

if bash "$installer" > "$work/out" 2>&1; then
  fail "installer ran without a mode"
fi
if bash "$installer" --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
  fail "installer accepted a fresh install without acknowledgement"
fi
grep -q -- '--fresh-host' "$work/out"

for args in '--tag v0.130.4' '--tag v3.8.0' '--tag v0.131.0-beta.0' \
            '--tag v0.131.0;touch /tmp/ftw-unexpected-install' '--tag' \
            '--fresh-host' '--resume' '--unknown' '--beta --tag v0.150.0'; do
  read -r -a words <<< "$args"
  if bash "$installer" --fresh-host "${words[@]}" > "$work/out" 2>&1; then
    fail "installer accepted invalid arguments: --fresh-host $args"
  fi
done
if bash "$installer" --resume --beta > "$work/out" 2>&1; then
  fail "installer accepted --beta with --resume"
fi
grep -q 'does not go with --tag or --resume' "$work/out"

# curl | bash: the script runs from a pipe, and a download cut short runs
# nothing. Any command main would call first is a stub that logs.
mkdir -p "$work/stubs"
for tool in uname curl sudo id systemctl mktemp ss; do
  printf '#!/bin/sh\necho %s >> "%s/calls"\n' "$tool" "$work" > "$work/stubs/$tool"
  chmod 0755 "$work/stubs/$tool"
done
# shellcheck disable=SC2002 # The point is a pipe, as with curl.
cat "$installer" | bash -s -- --help > "$work/out"
grep -q 'Usage: install.sh --fresh-host' "$work/out"
size="$(wc -c < "$installer" | tr -d ' ')"
last_line="$(tail -n 1 "$installer")"
cuts=()
for (( n = 1; n < size - 1; n += 53 )); do cuts+=("$n"); done
for (( n = size - ${#last_line} - 1; n < size - 1; n++ )); do cuts+=("$n"); done
for n in "${cuts[@]}"; do
  head -c "$n" "$installer" | PATH="$work/stubs:$PATH" bash -s -- --fresh-host > /dev/null 2>&1 || true
  if [[ -s "$work/calls" ]]; then
    fail "an installer cut after $n of $size bytes ran $(tr '\n' ' ' < "$work/calls")"
  fi
done

# Release lookup. These functions run anywhere, so load them without main.
(
  # shellcheck source=scripts/install.sh
  source "$installer"

  # A real GitHub page, saved with its headers: two Core releases with
  # packages and one other release.
  release_rows ftw-linux-arm64.tar.gz < "$data/github-releases-page.json" > "$work/rows"
  printf 'v0.140.11-beta.1\ttrue\tfalse\tyes\nha-export-v1-beta.1\ttrue\tfalse\tno\nv0.140.10-beta.1\ttrue\tfalse\tyes\n' > "$work/want"
  cmp "$work/want" "$work/rows" || fail "the parser misread the saved GitHub page"
  tr -d '\n' < "$data/github-releases-page.json" | release_rows ftw-linux-arm64.tar.gz > "$work/rows"
  cmp "$work/want" "$work/rows" || fail "the parser depends on the line layout"
  release_rows ftw-linux-riscv64.tar.gz < "$data/github-releases-page.json" | cut -f 4 | sort -u > "$work/rows"
  [[ "$(cat "$work/rows")" == no ]] || fail "the parser found a package for another architecture"
  next="$(next_page "$data/github-releases-page.headers" 'https://api.github.com/repos/srcfl/ftw/releases?per_page=3&page=20')"
  [[ "$next" == 'https://api.github.com/repositories/1208673229/releases?per_page=3&page=21' ]] ||
    fail "next_page read $next"
  if next_page "$data/github-releases-page.headers" 'https://example.test/releases' > /dev/null; then
    fail "next_page followed a link to another host"
  fi

  # Text that looks like JSON inside strings, names outside the asset list
  # and the other architecture's files must not count.
  cat > "$work/tricky.json" <<'EOF'
[{"tag_name":"v0.150.0","draft":false,"prerelease":false,
  "body":"not this: \"tag_name\": \"v9.0.0\", } ] { [ \\",
  "author":{"name":"ftw-linux-arm64.tar.gz"},
  "assets":[{"name":"ftw-linux-arm64.tar.gz","uploader":{"name":"ftw-linux-arm64.tar.gz.sha256"}},
            {"name":"ftw-linux-amd64.tar.gz.sha256","label":"ftw-linux-arm64.tar.gz.sha256"}]},
 {"tag_name":"v0.149.0","draft":false,"prerelease":false,"n":-1.5e3,"x":null,"y":[1,"a",{}],
  "assets":[{"name":"ftw-linux-arm64.tar.gz"},{"name":"ftw-linux-arm64.tar.gz.sha256"}]}]
EOF
  release_rows ftw-linux-arm64.tar.gz < "$work/tricky.json" > "$work/rows"
  printf 'v0.150.0\tfalse\tfalse\tno\nv0.149.0\tfalse\tfalse\tyes\n' | cmp - "$work/rows" ||
    fail "the parser was misled by text inside strings"

  head -c 30000 "$data/github-releases-page.json" > "$work/cut.json"
  for bad in "$work/cut.json" /dev/null; do
    if release_rows ftw-linux-arm64.tar.gz < "$bad" > /dev/null; then
      fail "the parser accepted $bad"
    fi
  done
  for bad in '{"message":"API rate limit exceeded"}' '[] []' '[{"tag_name":"v0.150.0"' '[["v0.150.0"]]' '"v0.150.0"'; do
    if printf '%s' "$bad" | release_rows ftw-linux-arm64.tar.gz > /dev/null; then
      fail "the parser accepted $bad"
    fi
  done

  # pick BETA COMPLETE ROWS...: rows are "tag prerelease draft package".
  pick() {
    local beta="$1" complete="$2"
    shift 2
    printf '%s\n' "$@" | tr ' ' '\t' | pick_release "$beta" "$complete" | tr '\t' ' '
  }
  expect() {
    local want="$1"
    shift
    local got
    got="$(pick "$@")"
    [[ "$got" == "$want" ]] || fail "pick_release $*: got '$got', want '$want'"
  }
  # A stable exists: it wins by default, and the first one listed is enough.
  expect 'v0.170.0 stable yes' no no 'v0.171.0-beta.1 true false yes' 'v0.170.0 false false yes'
  expect 'v0.171.0-beta.1 beta yes' yes no 'v0.171.0-beta.1 true false yes' 'v0.170.0 false false yes'
  # Only betas: the newest, once the whole list is read.
  expect 'more' no no 'v0.169.0-beta.1 true false yes' 'drivers-beta true false no'
  expect 'v0.169.0-beta.1 beta yes' no yes 'v0.169.0-beta.1 true false yes' 'v0.168.0-beta.1 true false yes' 'drivers-beta true false no'
  expect 'v0.169.0-beta.1 beta yes' yes no 'v0.169.0-beta.1 true false yes'
  expect 'v0.169.0-beta.10 beta yes' no yes 'v0.169.0-beta.9 true false yes' 'v0.169.0-beta.10 true false yes' 'v0.168.0-beta.11 true false yes'
  # A stable promoted after a newer beta: the stable by default, the beta
  # with --beta. A stable newer than every beta wins either way.
  expect 'v0.170.0 stable yes' no no 'v0.170.0 false false yes' 'v0.171.0-beta.1 true false yes' 'v0.170.0-beta.1 true false yes'
  expect 'v0.171.0-beta.1 beta yes' yes no 'v0.170.0 false false yes' 'v0.171.0-beta.1 true false yes' 'v0.170.0-beta.1 true false yes'
  expect 'v0.170.0 stable yes' yes no 'v0.170.0 false false yes' 'v0.170.0-beta.1 true false yes'
  # The pick reports a missing package; the installer then refuses it.
  expect 'v0.170.0 stable no' no no 'v0.170.0 false false no' 'v0.169.0 false false yes'
  # Drafts, mismatched prerelease flags, old lines and other tags never count.
  expect 'v0.169.0-beta.1 beta yes' no yes 'v0.180.0 false true yes' 'v0.179.0-beta.1 false false yes' \
    'v0.178.0 true false yes' 'v0.130.4 false false yes' 'v3.8.0 false false yes' 'v0.0200.0 false false yes' \
    'v0.169.0-beta.1 true false yes'
  expect 'none' yes yes 'drivers-stable false false no' 'drivers-beta true false no'

  # find_release reads GitHub page by page through a fake curl.
  fake="$work/fake"
  mkdir -p "$fake/bin" "$fake/pages"
  cat > "$fake/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
out="" headers="" url=""
while (( $# > 0 )); do
  case "$1" in
    -o) out="$2"; shift ;;
    -D) headers="$2"; shift ;;
    -w|-H|--retry|--retry-max-time|--connect-timeout|--max-time|--proto|--proto-redir) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
echo "$url" >> "$FAKE/log"
if [[ -e "$FAKE/offline" ]]; then
  echo "curl: (6) Could not resolve host" >&2
  exit 6
fi
page="$FAKE/pages/${url##*page=}"
cp "$page.json" "$out"
if [[ -e "$page.headers" ]]; then cp "$page.headers" "$headers"; else printf 'HTTP/2 200\r\n\r\n' > "$headers"; fi
if [[ -e "$page.status" ]]; then cat "$page.status"; else printf 200; fi
EOF
  chmod 0755 "$fake/bin/curl"
  release() {  # tag prerelease
    printf '{"tag_name":"%s","draft":false,"prerelease":%s,"assets":[{"name":"ftw-linux-arm64.tar.gz"},{"name":"ftw-linux-arm64.tar.gz.sha256"}]}' "$1" "$2"
  }
  link_to() {
    printf 'HTTP/2 200\r\nlink: <https://list.test/releases?per_page=2&page=%s>; rel="next", <https://list.test/releases?per_page=2&page=9>; rel="last"\r\n\r\n' "$1"
  }
  lookup() {
    rm -f "$fake/log" "$fake/offline"
    FAKE="$fake" PATH="$fake/bin:$PATH" find_release "$1" ftw-linux-arm64.tar.gz 'https://list.test/releases?per_page=2&page=1' "$work" 2> "$work/err" | tr '\t' ' '
  }
  printf '[%s,%s]' "$(release v0.171.0-beta.1 true)" "$(release v0.170.0-beta.1 true)" > "$fake/pages/1.json"
  link_to 2 > "$fake/pages/1.headers"
  printf '[%s,%s]' "$(release v0.170.0 false)" "$(release v0.169.0-beta.1 true)" > "$fake/pages/2.json"
  [[ "$(lookup no)" == 'v0.170.0 stable yes' ]] || fail "find_release missed the stable on page 2"
  [[ "$(wc -l < "$fake/log" | tr -d ' ')" == 2 ]] || fail "find_release read $(wc -l < "$fake/log") pages, want 2"
  [[ "$(lookup yes)" == 'v0.171.0-beta.1 beta yes' ]] || fail "find_release --beta missed the newest beta"
  [[ "$(wc -l < "$fake/log" | tr -d ' ')" == 1 ]] || fail "find_release --beta read past the first beta"
  printf '[%s]' "$(release v0.169.0-beta.1 true)" > "$fake/pages/2.json"
  [[ "$(lookup no)" == 'v0.171.0-beta.1 beta yes' ]] || fail "find_release missed the newest beta with no stable"

  failing() {
    local want="$1"
    if lookup no > /dev/null; then fail "find_release did not fail: $want"; fi
    grep -q -- "$want" "$work/err" || fail "find_release said $(cat "$work/err"), want $want"
    grep -q -- '--tag' "$work/err" || fail "find_release did not mention --tag"
  }
  for status in 403 429; do
    printf %s "$status" > "$fake/pages/1.status"
    failing "HTTP $status.*an hour"
  done
  printf 500 > "$fake/pages/1.status"
  failing "HTTP 500"
  rm "$fake/pages/1.status"
  printf '[%s' "$(release v0.169.0-beta.1 true)" > "$fake/pages/2.json"
  failing 'cut off'
  link_to 1 > "$fake/pages/1.headers"
  failing "does not end"
  [[ "$(wc -l < "$fake/log" | tr -d " ")" == 1 ]] || fail "find_release read a repeated page"
  printf 'HTTP/2 200\r\nlink: <https://elsewhere.test/releases?page=2>; rel="next"\r\n\r\n' > "$fake/pages/1.headers"
  failing 'another host'
  rm -f "$fake/log"
  touch "$fake/offline"
  if FAKE="$fake" PATH="$fake/bin:$PATH" find_release no ftw-linux-arm64.tar.gz 'https://list.test/releases?page=1' "$work" 2> "$work/err"; then
    fail "find_release did not fail offline"
  fi
  grep -q 'did not answer' "$work/err" || fail "find_release offline said $(cat "$work/err")"
)

clean_host() {
  [[ ! -e /opt/ftw && ! -e /var/lib/ftw && ! -e /etc/ftw-native-install.pending ]] &&
    ! id ftw >/dev/null 2>&1 &&
    ! { command -v ss >/dev/null 2>&1 && ss -ltnH | awk '$4 ~ /:8080$/ { found = 1 } END { exit !found }'; }
}

if [[ "$(uname -s)" == Linux && ! -d /run/systemd/system ]]; then
  # A container or WSL without systemd: the installer says so before anything else.
  if bash "$installer" --resume > "$work/out" 2>&1; then
    fail "installer ran without systemd"
  fi
  grep -q 'does not run this computer\|has no systemd' "$work/out" ||
    fail "installer did not say systemd is missing: $(cat "$work/out")"
elif [[ "$(uname -s)" == Linux ]]; then
  for args in '--resume --tag v0.131.0-beta.1' '--resume'; do
    read -r -a words <<< "$args"
    if bash "$installer" "${words[@]}" > "$work/out" 2>&1; then
      fail "installer resumed without a pending install: $args"
    fi
    grep -q 'No interrupted native install found' "$work/out"
  done
  mkdir -p "$work/home/ftw"
  ln -s /no-such-compose "$work/home/ftw/docker-compose.yml"
  for args in '--fresh-host --tag v0.131.0-beta.1' '--fresh-host'; do
    read -r -a words <<< "$args"
    if HOME="$work/home" bash "$installer" "${words[@]}" > "$work/out" 2>&1; then
      fail "installer accepted an existing Compose path: $args"
    fi
    grep -q 'Existing FTW installation found' "$work/out"
    if grep -Eqi 'sudo|download|checksum|release' "$work/out"; then
      fail "installer did not stop before privilege, lookup or download: $args"
    fi
  done
  if clean_host; then
    # The newest release has no package yet: refuse before any download.
    rm -f "$work/fake/offline"
    printf '[%s]' '{"tag_name":"v0.170.0","draft":false,"prerelease":false,"assets":[]}' > "$work/fake/pages/9.json"
    mkdir -p "$work/home2"
    if HOME="$work/home2" FAKE="$work/fake" PATH="$work/fake/bin:$PATH" \
       FTW_RELEASES_URL='https://list.test/releases?page=9' bash "$installer" --fresh-host > "$work/out" 2>&1; then
      fail "installer accepted a release without a package"
    fi
    grep -q 'v0.170.0 has no ftw-linux-' "$work/out" || fail "installer said $(cat "$work/out")"
    if grep -q 'Downloading' "$work/out"; then
      fail "installer downloaded a release without a package"
    fi
  fi
fi

if [[ "$(uname -s)" == Linux && -d /run/systemd/system && ! -e /opt/ftw/slots.json ]]; then
  for args in '--refresh --tag v0.131.0-beta.1' '--refresh'; do
    read -r -a words <<< "$args"
    if bash "$installer" "${words[@]}" > "$work/out" 2>&1; then
      fail "installer refreshed a host without a native install: $args"
    fi
    grep -q 'No native install made by this script was found' "$work/out"
    if grep -Eqi 'sudo|download|checksum|release' "$work/out"; then
      fail "refresh did not stop before privilege, lookup or download: $args"
    fi
  done
fi

echo 'native installer guards and release lookup passed'
