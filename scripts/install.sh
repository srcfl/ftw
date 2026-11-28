#!/usr/bin/env bash
# Install a published native 0.x release on a fresh 64-bit Linux computer.
# Existing sites switch through separate setups; this installer does not migrate data.
#
#   curl -fsSL https://ftw.energy/install.sh | bash -s -- --fresh-host
#
# The file only defines functions until its last line calls main, so a
# download that stops partway runs nothing.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: install.sh --fresh-host [--tag v0.X.Y[-beta.N] | --beta]
       install.sh --resume [--tag v0.X.Y[-beta.N]]
       install.sh --refresh [--tag v0.X.Y[-beta.N] | --beta]

Installs FTW on a 64-bit Linux computer that runs systemd.

  --fresh-host  Install on a computer with no FTW site, not even a stopped
                Docker site in a custom directory.
  --resume      Finish an install that stopped partway, with its release.
  --refresh     Replace the launcher, the ftw command and the service file
                of an install this script made. ftw update moves Core.
  --tag         Use this published release.
  --beta        Use the newest beta when it is newer than the newest stable.

Without --tag the installer takes the newest stable release, or the newest
beta while there is no stable.

It never replaces a site or updates a Docker container. 2.x and 3.x get no
more updates; to switch from them, see
https://github.com/srcfl/ftw/blob/master/docs/native-beta.md
EOF
}

usage_error() {
  usage >&2
  exit 2
}

native_tag_re='^v0\.([1-9][0-9]*)\.(0|[1-9][0-9]*)(-beta\.([1-9][0-9]*))?$'

# A tag on the native line, v0.131.0 or later.
native_tag() {
  [[ "$1" =~ $native_tag_re ]] && (( BASH_REMATCH[1] >= 131 ))
}

# Reads a GitHub release list (JSON) and prints one line per release: tag,
# prerelease, draft, and yes when it has the package $1 and its .sha256,
# separated by tabs. Fails unless the input is one complete JSON array.
# A JSON string holds no raw newline, so grep can cut any layout into
# tokens; awk then tracks where each token sits.
release_rows() {
  LC_ALL=C grep -oE '"([^"\\]|\\.)*"|[][{}:,]|-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?|true|false|null' |
    LC_ALL=C awk -v archive="$1" '
      function fail() { bad = 1; exit }
      {
        c = substr($0, 1, 1)
        if (c == "{" || c == "[") {
          if (done || (d == 0 && c != "[") || (d == 1 && c != "{")) fail()
          d++; type[d] = c; key[d] = ""; wantkey[d] = (c == "{")
          if (d == 2) { tag = ""; pre = ""; draft = ""; pkg = 0; sum = 0 }
          next
        }
        if (c == "}" || c == "]") {
          if (d == 0 || (c == "}") != (type[d] == "{")) fail()
          if (d == 2) print tag "\t" pre "\t" draft "\t" (pkg && sum ? "yes" : "no")
          if (--d == 0) done = 1
          next
        }
        if (c == ":") { wantkey[d] = 0; next }
        if (c == ",") { wantkey[d] = (type[d] == "{"); next }
        if (d == 0) fail()
        text = (c == "\"") ? substr($0, 2, length($0) - 2) : $0
        if (type[d] == "{" && wantkey[d]) { key[d] = text; next }
        if (d == 2 && key[2] == "tag_name") tag = text
        else if (d == 2 && key[2] == "prerelease") pre = text
        else if (d == 2 && key[2] == "draft") draft = text
        else if (d == 4 && key[2] == "assets" && key[4] == "name") {
          if (text == archive) pkg = 1
          if (text == archive ".sha256") sum = 1
        }
      }
      END { exit (bad || !done) }'
}

# Reads release lines in GitHub's order and prints the release to install:
# tag, stable or beta, and whether it has the package. It prints "more"
# while a later page could change the answer, and "none" when no native
# release fits. $1 is yes for --beta; $2 is yes when the list is complete.
#
# This follows Core's update check (resolveChannel in
# go/internal/selfupdate/selfupdate.go). GitHub lists the newest release
# first, and scripts/check-native-release-order.py refuses a stable below a
# published stable and a beta below any published release. So the first
# stable listed is the newest stable, and nothing after the first beta
# outranks it.
pick_release() {
  LC_ALL=C awk -F '\t' -v beta="$1" -v complete="$2" '
    $3 == "true" || $1 !~ /^v0\.[1-9][0-9]*\.(0|[1-9][0-9]*)(-beta\.[1-9][0-9]*)?$/ { next }
    {
      split(substr($1, 2), v, /[.-]/)
      if (v[2] + 0 < 131) next
      stable = ($1 !~ /-beta\./)
      if (stable != ($2 == "false")) next
      rank = sprintf("r%09d%09d%d%09d", v[2], v[3], stable, stable ? 0 : v[5])
      if (stable) {
        if (s == "") { s = $1; srank = rank; sok = $4 }
        if (beta != "yes") { found = 1; exit }
      } else {
        if (b == "" || rank > brank) { b = $1; brank = rank; bok = $4 }
        if (beta == "yes") { found = 1; exit }
      }
    }
    END {
      if (!found && complete != "yes") print "more"
      else if (s != "" && (beta != "yes" || b == "" || srank > brank)) print s "\tstable\t" sok
      else if (b != "") print b "\tbeta\t" bok
      else print "none"
    }'
}

# Prints the next page that GitHub's Link header in file $1 names, if any.
# Fails when it leaves the scheme and host of page $2, as Core does.
next_page() {
  local next origin_re='^[A-Za-z][A-Za-z0-9+.-]*://[^/?#]*'
  next="$(LC_ALL=C awk '
    /^HTTP\// { link = "" }
    {
      line = $0
      sub(/\r$/, "", line)
      if (tolower(substr(line, 1, 5)) == "link:") link = substr(line, 6)
    }
    END {
      n = split(link, parts, ",")
      for (i = 1; i <= n; i++)
        if (parts[i] ~ /rel="next"/ && match(parts[i], /<[^>]*>/)) {
          print substr(parts[i], RSTART + 1, RLENGTH - 2)
          exit
        }
    }' "$1")"
  [[ -n "$next" ]] || return 0
  [[ "$next" =~ $origin_re ]] || return 1
  local next_origin="${BASH_REMATCH[0]}"
  [[ "$2" =~ $origin_re && "${BASH_REMATCH[0]}" == "$next_origin" ]] || return 1
  printf '%s\n' "$next"
}

list_failed() {
  echo "Could not find the newest FTW release: $1." >&2
  echo "Try again later, or name a release from https://github.com/srcfl/ftw/releases with --tag v0.X.Y." >&2
}

# Prints the release to install (see pick_release), reading GitHub's
# release list page by page from $3 into directory $4. $1 is yes for
# --beta and $2 the package name.
find_release() {
  local beta="$1" archive="$2" page="$3" work="$4" seen=" " pages=0
  local status next complete answer
  local protocols=(--proto '=https' --proto-redir '=https')
  if [[ -n "${FTW_RELEASES_URL:-}" ]]; then
    protocols=()
  fi
  : > "$work/release-rows"
  while :; do
    pages=$((pages + 1))
    if (( pages > 20 )) || [[ "$seen" == *" $page "* ]]; then
      list_failed "the release list does not end"
      return 1
    fi
    seen="$seen$page "
    if ! status="$(curl --silent --show-error --location --retry 3 --retry-max-time 60 \
        --connect-timeout 20 --max-time 300 ${protocols[@]+"${protocols[@]}"} \
        -H 'Accept: application/vnd.github+json' -D "$work/release-headers" \
        -o "$work/release-page" -w '%{http_code}' "$page")"; then
      list_failed "GitHub did not answer"
      return 1
    fi
    case "$status" in
      200) ;;
      403|429)
        list_failed "GitHub refused (HTTP $status); it allows 60 lookups an hour from one address"
        return 1 ;;
      *)
        list_failed "GitHub answered HTTP $status"
        return 1 ;;
    esac
    if ! release_rows "$archive" < "$work/release-page" >> "$work/release-rows"; then
      list_failed "the release list was cut off or not understood"
      return 1
    fi
    if ! next="$(next_page "$work/release-headers" "$page")"; then
      list_failed "the next page of the release list is on another host"
      return 1
    fi
    complete=no
    [[ -n "$next" ]] || complete=yes
    answer="$(pick_release "$beta" "$complete" < "$work/release-rows")"
    [[ "$answer" == more ]] || break
    page="$next"
  done
  if [[ "$answer" == none ]]; then
    list_failed "GitHub lists no native release"
    return 1
  fi
  printf '%s\n' "$answer"
}

docs_url=https://github.com/srcfl/ftw/blob/master/docs/install.md

# The line after a refusal that leaves no way forward on this computer.
other_ways() {
  echo "On a Raspberry Pi, use Raspberry Pi OS Lite (64-bit). With Home Assistant, use the FTW app for Home Assistant. Every way to run FTW: $docs_url" >&2
}

# Prints the package that holds tool $1.
tool_package() {
  case "$1" in
    sha256sum|mktemp) echo coreutils ;;
    awk) echo gawk ;;
    *) echo "$1" ;;
  esac
}

# Prints the package manager this computer uses: apt, dnf, zypper or pacman.
package_family() {
  local family
  for family in apt-get dnf zypper pacman; do
    if command -v "$family" >/dev/null 2>&1; then
      echo "${family%-get}"
      return 0
    fi
  done
}

# Prints the command that installs packages $3... with package manager $1,
# each step led by $2 (sudo or nothing). It prints nothing for another one.
install_command() {
  local family="$1" lead="$2"
  shift 2
  case "$family" in
    apt) echo "${lead}apt update && ${lead}apt install -y $*" ;;
    dnf) echo "${lead}dnf install -y $*" ;;
    zypper) echo "${lead}zypper install -y $*" ;;
    pacman) echo "${lead}pacman -S --needed $*" ;;
  esac
}

# Joins words as a person would: "a", "a and b", "a, b and c".
join_words() {
  local text="$1"
  shift
  while (( $# > 1 )); do
    text="$text, $1"
    shift
  done
  (( $# == 0 )) || text="$text and $1"
  printf '%s\n' "$text"
}

# Stops with the command that adds any tool the installer needs. tar needs
# gzip for the package.
check_tools() {
  local tool package lead=""
  local tools=(curl tar gzip sha256sum grep awk mktemp) missing=() packages=()
  if (( EUID != 0 )); then
    tools+=(sudo)
  fi
  for tool in "${tools[@]}"; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      missing+=("$tool")
      package="$(tool_package "$tool")"
      if [[ " ${packages[*]-} " != *" $package "* ]]; then
        packages+=("$package")
      fi
    fi
  done
  (( ${#missing[@]} > 0 )) || return 0
  local them=them command
  (( ${#missing[@]} > 1 )) || them=it
  if (( EUID != 0 )) && command -v sudo >/dev/null 2>&1; then
    lead="sudo "
  fi
  command="$(install_command "$(package_family)" "$lead" "${packages[@]}")"
  echo "The installer needs $(join_words "${missing[@]}"), which this computer lacks." >&2
  if [[ -z "$command" ]]; then
    echo "Add $them with this computer's package manager (packages: $(join_words "${packages[@]}")), then run the line again." >&2
  elif (( EUID != 0 )) && [[ -z "$lead" ]]; then
    echo "Add $them as root with this command, then run the line again, or run the line itself as root:" >&2
    echo "  $command" >&2
  else
    echo "Add $them with this command, then run the line again:" >&2
    echo "  $command" >&2
  fi
  exit 2
}

# Prints the MB free in the file system that holds $1, or its nearest
# parent that exists; nothing when df cannot tell.
free_mb() {
  local path="$1"
  while [[ ! -e "$path" && "$path" != / ]]; do
    path="$(dirname "$path")"
  done
  df -Pm "$path" 2>/dev/null | awk 'NR == 2 && $4 ~ /^[0-9]+$/ { print $4 }' || true
}

# Prints the free space for $1 in words, also when df cannot tell.
space_of() {
  local free
  free="$(free_mb "$1")"
  if [[ -n "$free" ]]; then
    echo "$free MB"
  else
    echo "unknown (df could not check it)"
  fi
}

# True unless df shows less than $2 MB free for $1.
has_room() {
  local free
  free="$(free_mb "$1")"
  [[ -z "$free" ]] || (( free >= $2 ))
}

# Prints the folder to unpack the download in, for $1 MB. A tmpfs keeps its
# files in memory; when it is small, as on a Raspberry Pi 3, /var/tmp on
# disk comes first.
staging_parent() {
  local base="${TMPDIR:-/tmp}" need="$1" move=no
  if ! has_room "$base" "$need" ||
     { [[ "$(stat -f -c %T "$base" 2>/dev/null)" == tmpfs ]] && ! has_room "$base" 1024; }; then
    move=yes
  fi
  if [[ "$move" == yes && "$base" != /var/tmp && -d /var/tmp && -w /var/tmp ]] && has_room /var/tmp "$need"; then
    base=/var/tmp
  fi
  has_room "$base" "$need" || return 1
  printf '%s\n' "$base"
}

# True when a program listens on TCP port $1.
port_busy() {
  local sockets
  # An ss that fails, perhaps too old for -H, leaves the kernel's tables.
  if command -v ss >/dev/null 2>&1 && sockets="$(ss -ltnH 2>/dev/null)"; then
    printf '%s\n' "$sockets" |
      awk -v port=":$1" 'substr($4, length($4) - length(port) + 1) == port { found = 1 } END { exit !found }'
  else
    proc_listening "$1" /proc/net
  fi
}

# True when the kernel's socket tables, tcp and tcp6 in folder $2, show a
# program listening on TCP port $1. Minimal systems may lack ss.
proc_listening() {
  local dir="$2" file files=()
  for file in "$dir/tcp" "$dir/tcp6"; do
    if [[ -r "$file" ]]; then
      files+=("$file")
    fi
  done
  (( ${#files[@]} > 0 )) || return 1
  # Each row is "slot local remote state ...", the local port in hex after
  # the colon, and state 0A is listening.
  LC_ALL=C awk -v port="$(printf '%04X' "$1")" '
    $4 == "0A" && substr($2, length($2) - 3) == port { found = 1 }
    END { exit !found }' "${files[@]}"
}

# Prints the local IPv4 addresses in a copy of /proc/net/fib_trie ($1).
fib_trie_addresses() {
  [[ -r "$1" ]] || return 0
  LC_ALL=C awk '/\/32 host LOCAL/ { print last } { last = $2 }' "$1"
}

# Reads IPv4 addresses and prints, once each, those others on the network
# can reach: not loopback, not link-local and not on a container bridge. The
# routes in /proc/net/route, or in $1 for tests, name each network's
# interface, as ip does.
lan_only() {
  local routes="${1:-/proc/net/route}"
  [[ -r "$routes" ]] || routes=/dev/null
  LC_ALL=C awk -v rf="$routes" '
    # The kernel prints each address as a little-endian hex number.
    function byte(hex, i,   digits) {
      digits = "0123456789ABCDEF"
      hex = toupper(substr(hex, 9 - 2 * i, 2))
      return (index(digits, substr(hex, 1, 1)) - 1) * 16 + index(digits, substr(hex, 2, 1)) - 1
    }
    FILENAME == rf {
      if ($1 ~ /^(docker|br-|veth|virbr|cni|podman)/ && length($8) == 8 && $8 ~ /^[0-9A-Fa-f]+$/ && $8 != "00000000") {
        n++
        for (i = 1; i <= 4; i++) { net[n, i] = byte($2, i); mask[n, i] = byte($8, i) }
      }
      next
    }
    $1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ && $1 !~ /^(127|169\.254)\./ && !seen[$1]++ {
      split($1, a, ".")
      for (r = 1; r <= n; r++) {
        inside = 1
        for (i = 1; i <= 4; i++)
          if (a[i] - a[i] % (256 - mask[r, i]) != net[r, i]) inside = 0
        if (inside) next
      }
      print $1
    }' "$routes" -
}

# Each IPv4 address that others on the network can reach. Minimal systems
# may lack ip and hostname -I, so the kernel's own tables come next.
ipv4_addresses() {
  local found=""
  if command -v ip >/dev/null 2>&1; then
    found="$(ip -4 -o addr show scope global 2>/dev/null |
      awk '$2 !~ /^(docker|br-|veth|virbr|cni|podman)/ { sub(/\/.*/, "", $4); print $4 }')" || true
  fi
  if [[ -z "$found" ]] && command -v hostname >/dev/null 2>&1; then
    found="$(hostname -I 2>/dev/null | tr ' ' '\n' | lan_only)" || true
  fi
  if [[ -z "$found" ]]; then
    found="$(fib_trie_addresses /proc/net/fib_trie | lan_only)" || true
  fi
  if [[ -z "$found" ]] && command -v getent >/dev/null 2>&1; then
    found="$(getent ahostsv4 "$(uname -n)" 2>/dev/null | awk '{ print $1 }' | lan_only)" || true
  fi
  [[ -z "$found" ]] || printf '%s\n' "$found"
}

# Prints the .local name that Avahi or systemd-resolved announces for this
# computer, if one does. Core announces no name itself. Avahi may have
# picked another name than the hostname when two computers share it.
mdns_name() {
  local reply name=""
  command -v busctl >/dev/null 2>&1 || return 1
  if systemctl is-active --quiet avahi-daemon.service 2>/dev/null &&
     reply="$(busctl --system --auto-start=no --timeout=2 call org.freedesktop.Avahi / \
       org.freedesktop.Avahi.Server GetState 2>/dev/null)" && [[ "$reply" == "i 2" ]] &&
     reply="$(busctl --system --auto-start=no --timeout=2 call org.freedesktop.Avahi / \
       org.freedesktop.Avahi.Server GetHostNameFqdn 2>/dev/null)"; then
    name="${reply#s \"}"
    name="${name%\"}"
  elif systemctl is-active --quiet systemd-resolved.service 2>/dev/null &&
     reply="$(busctl --system --auto-start=no --timeout=2 get-property org.freedesktop.resolve1 \
       /org/freedesktop/resolve1 org.freedesktop.resolve1.Manager MulticastDNS 2>/dev/null)" &&
     [[ "$reply" == 's "yes"' ]] &&
     resolvectl mdns 2>/dev/null | grep -qE '^Link .*: yes$' &&
     reply="$(busctl --system --auto-start=no --timeout=2 get-property org.freedesktop.resolve1 \
       /org/freedesktop/resolve1 org.freedesktop.resolve1.Manager LLMNRHostname 2>/dev/null)"; then
    name="${reply#s \"}"
    name="${name%\"}.local"
  fi
  [[ "$name" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.local$ ]] || return 1
  printf '%s\n' "$name"
}

# Prints the addresses of page $1 on this computer's port 8080.
setup_addresses() {
  local page="$1" ip name found=no
  while IFS= read -r ip; do
    [[ -n "$ip" ]] || continue
    echo "  http://$ip:8080$page"
    found=yes
  done < <(ipv4_addresses)
  if name="$(mdns_name)"; then
    echo "  http://$name:8080$page"
    found=yes
  fi
  if [[ "$found" == no ]]; then
    echo "  This computer did not report its address. Find it in your router's list of"
    echo "  devices, then open http://<address>:8080$page with that address."
  fi
}

# FTW_INSTALL_TEST_ROOT moves the paths that the next three functions read,
# so tests can build a box in a folder. main clears it; leave it unset.

# True when this script's own install is here: the release folder and the
# service that starts its launcher.
own_install() {
  local root="${FTW_INSTALL_TEST_ROOT:-}"
  [[ -d "$root/opt/ftw" && -f "$root/etc/systemd/system/ftw.service" ]] &&
    grep -q '^ExecStart=/opt/ftw/ftw-launcher -root /opt/ftw ' "$root/etc/systemd/system/ftw.service"
}

# Says that FTW is installed already, where to open it and how to update
# it, then exits: 0 while the service runs or starts, 1 when it does not.
already_installed() {
  local root="${FTW_INSTALL_TEST_ROOT:-}" state sub health page=/ status=0 update
  state="$(systemctl is-active ftw.service 2>/dev/null || true)"
  # A service that waits to start again after a crash counts as activating.
  sub="$(systemctl show -p SubState --value ftw.service 2>/dev/null || true)"
  # 000 when nothing answers. Until setup is done, Core serves only the
  # setup page, so it answers 404 here.
  health="$(curl -s -o /dev/null -m 5 -w '%{http_code}' http://127.0.0.1:8080/api/health 2>/dev/null || true)"
  # Without D-Bus, systemctl tells only root; Core's own answer counts too.
  if [[ -z "$state" && -n "$health" && "$health" != 000 ]]; then
    state=active
  fi
  if [[ "$sub" == auto-restart ]]; then
    state=crashing
  fi
  case "$state" in
    active)
      if [[ "$health" == 404 ]]; then
        page=/setup
        echo "FTW is already installed on this computer and running. It waits for setup;"
        echo "open one of these from a phone or computer on the same network:"
      else
        echo "FTW is already installed on this computer and running. Open it from a phone"
        echo "or computer on the same network:"
      fi ;;
    activating|reloading)
      echo "FTW is already installed on this computer and starting. In a minute, open it"
      echo "from a phone or computer on the same network:" ;;
    crashing)
      status=1
      echo "FTW is already installed on this computer, but it keeps stopping;"
      echo "${sudo_word}journalctl -u ftw -n 100 shows why. Once it runs, open it from a phone"
      echo "or computer on the same network:" ;;
    *)
      status=1
      echo "FTW is already installed on this computer, but it is not running."
      echo "Start it with ${sudo_word}systemctl start ftw; ${sudo_word}journalctl -u ftw -n 100 shows why it stopped."
      echo "Then open it from a phone or computer on the same network:" ;;
  esac
  setup_addresses "$page"
  echo "Nothing was changed."
  if [[ -x "$root/usr/local/bin/ftw" ]]; then
    echo "On this computer, ftw status shows how FTW is doing and ftw update installs"
    echo "a newer release."
    update="ftw update"
  else
    echo "On this computer, systemctl status ftw shows how the service is doing."
    update="More → Version → Update FTW on the box's page"
  fi
  if [[ "${tag_given:-no}" == yes || "${beta:-no}" == yes ]]; then
    echo "--tag and --beta change nothing on an installed box; use $update instead."
  fi
  exit "$status"
}

# The service account that this script made: a system user at home in
# /var/lib/ftw.
own_account() {
  [[ "$(getent passwd ftw 2>/dev/null | cut -d: -f6)" == /var/lib/ftw ]]
}

# Stops on what the documented removal leaves behind, /var/lib/ftw and the
# ftw account, when nothing else of FTW is here. $@ are more paths that
# mean another install.
check_leftovers() {
  local root="${FTW_INSTALL_TEST_ROOT:-}" path data=no account=no
  for path in "$root/opt/ftw" "$root/usr/local/bin/ftw" \
      "$root"{/etc,/lib,/usr/lib}/systemd/system/{ftw,forty-two-watts}.service "$@"; do
    if [[ -e "$path" || -L "$path" ]]; then
      return 0
    fi
  done
  if systemctl cat ftw.service >/dev/null 2>&1 || systemctl cat forty-two-watts.service >/dev/null 2>&1; then
    return 0
  fi
  if [[ -d "$root/var/lib/ftw" && ! -L "$root/var/lib/ftw" ]]; then
    data=yes
  fi
  if own_account; then
    account=yes
  fi
  [[ "$data" == yes || "$account" == yes ]] || return 0
  if [[ "$data" == yes ]]; then
    echo "FTW was removed from this computer, but its data is still in /var/lib/ftw:" >&2
    echo "the settings and history of the old install. The installer does not write over it." >&2
    echo "If you may want it again, back it up first. The copy holds the box's keys, so" >&2
    echo "this command makes it readable only by you:" >&2
    echo "  (umask 077; ${sudo_word}tar -czf - -C /var/lib ftw > ~/ftw-data-backup.tar.gz)" >&2
    echo "Then remove what is left and run the install line again:" >&2
    echo "  ${sudo_word}rm -rf /var/lib/ftw" >&2
  else
    echo "FTW was removed from this computer, but its ftw account is still here." >&2
    echo "Remove it and run the install line again:" >&2
  fi
  if [[ "$account" == yes ]]; then
    echo "  ${sudo_word}userdel ftw" >&2
  fi
  exit 2
}

main() {
  unset FTW_INSTALL_TEST_ROOT
  mode=""
  tag=""
  tag_given=no
  beta=no
  while (( $# > 0 )); do
    case "$1" in
      -h|--help) usage; return 0 ;;
      --fresh-host|--resume|--refresh)
        [[ -z "$mode" ]] || usage_error
        mode="$1" ;;
      --tag)
        [[ $# -ge 2 && "$tag_given" == no ]] || usage_error
        tag="$2"
        tag_given=yes
        shift ;;
      --beta) beta=yes ;;
      *) usage_error ;;
    esac
    shift
  done
  [[ -n "$mode" ]] || usage_error
  if [[ "$beta" == yes && ( "$tag_given" == yes || "$mode" == --resume ) ]]; then
    echo "--beta picks the newest release, so it does not go with --tag or --resume." >&2
    exit 2
  fi
  if [[ "$tag_given" == yes ]] && ! native_tag "$tag"; then
    echo "A native release tag v0.131.0 or later is required: $tag" >&2
    exit 2
  fi

  # What a person types before a command that needs root.
  sudo_word="sudo "
  if (( EUID == 0 )); then
    sudo_word=""
  fi

  os="$(uname -s)"
  if [[ "$os" != Linux ]]; then
    [[ "$os" != Darwin ]] || os=macOS
    echo "FTW installs on Linux with systemd, and this computer runs $os." >&2
    other_ways
    exit 2
  fi
  case "$(uname -m)" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *)
      echo "FTW needs a 64-bit ARM or AMD64 system, and this one is $(uname -m)." >&2
      other_ways
      exit 2 ;;
  esac
  check_tools
  # FTW runs as a systemd service. Most containers, and WSL unless told
  # otherwise, start another program as process 1.
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "This computer has no systemd. FTW runs as a systemd service; use Raspberry Pi OS, Debian, Ubuntu or another Linux that starts with systemd." >&2
    other_ways
    exit 2
  fi
  if [[ ! -d /run/systemd/system ]]; then
    init="$(cat /proc/1/comm 2>/dev/null || echo unknown)"
    echo "systemd is installed, but it does not run this computer: process 1 is $init. This is usual in a container, and in WSL without systemd turned on. FTW runs as a systemd service, so install it on a computer or virtual machine that starts with systemd." >&2
    other_ways
    exit 2
  fi

  pending=/etc/ftw-native-install.pending
  # These checks catch common layouts. --fresh-host also covers a stopped
  # Docker site in a custom FTW_DIR that cannot be found from a fixed path.
  legacy_paths=(
    "$HOME/ftw/docker-compose.yml"
    "$HOME/forty-two-watts/docker-compose.yml"
  )
  installed_paths=(/opt/ftw/slots.json /opt/ftw/ftw-launcher /etc/systemd/system/ftw.service)
  if [[ "$mode" == --refresh ]]; then
    existing_paths=()
    for path in "${installed_paths[@]}"; do
      if [[ ! -e "$path" ]]; then
        echo "No native install made by this script was found ($path is missing); --refresh changes nothing." >&2
        exit 2
      fi
    done
  elif [[ "$mode" == --fresh-host ]]; then
    if [[ -e "$pending" || -L "$pending" ]]; then
      echo "An earlier install on this computer stopped partway. To finish it, run the same command again with --resume in place of --fresh-host. Its data stays in place." >&2
      exit 2
    fi
    # Running the line again on a box it set up is no error. Other layouts,
    # Docker or 3.x among them, get the refusal below.
    if own_install; then
      already_installed
    fi
    check_leftovers "${legacy_paths[@]}"
    existing_paths=(/opt/ftw /var/lib/ftw /etc/systemd/system/ftw.service /usr/local/bin/ftw
      /etc/systemd/system/forty-two-watts.service "$pending" "${legacy_paths[@]}")
  else
    existing_paths=("${legacy_paths[@]}")
    if [[ ! -f "$pending" ]]; then
      echo "No interrupted native install found at $pending; --resume cannot start a new site." >&2
      exit 2
    fi
  fi
  for path in ${existing_paths[@]+"${existing_paths[@]}"}; do
    if [[ -e "$path" || -L "$path" ]]; then
      echo "Existing FTW installation found at $path. Keep its data. Use a new card/host or separate Docker setup as described in https://github.com/srcfl/ftw/blob/master/docs/native-beta.md; only one Core may control the equipment." >&2
      exit 2
    fi
  done
  if [[ "$mode" != --refresh ]] && {
     systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
     systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1 ||
     { [[ "$mode" == --fresh-host ]] &&
       { systemctl cat ftw.service >/dev/null 2>&1 ||
         systemctl cat forty-two-watts.service >/dev/null 2>&1; }; }; }; then
    echo "An FTW service already exists; refusing a fresh install." >&2
    exit 2
  fi
  # Cards made from the old FTW image logged in as ftw, so people pick it again.
  if [[ "$mode" == --fresh-host ]] && id ftw >/dev/null 2>&1; then
    echo "A user named ftw already exists. FTW runs as its own ftw account, so install from a login with another name; on a fresh Raspberry Pi card, choose a different username. Keep any card containing an existing FTW site and follow docs/native-beta.md." >&2
    exit 2
  fi
  if [[ "$mode" != --refresh ]] && port_busy 8080; then
    echo "Port 8080 is already in use; refusing a fresh install." >&2
    exit 2
  fi

  if (( EUID == 0 )); then
    as_root() { "$@"; }
  else
    if ! command -v sudo >/dev/null 2>&1; then
      echo "sudo is required to install the service." >&2
      exit 2
    fi
    as_root() { sudo "$@"; }
  fi
  # sudo asks on the terminal, not on stdin, so this works when the script
  # comes through a pipe.
  sudo_ready() {
    if (( EUID != 0 )) && ! sudo -n true 2>/dev/null && ! sudo -v; then
      echo "sudo gave no root access. Run the installer in a terminal, as a user who may use sudo, or as root." >&2
      exit 2
    fi
  }

  # The download and its unpacked release take about 120 MB while they are
  # checked, and /opt/ftw keeps the release and the next update's.
  need_mb=250
  if ! staging="$(staging_parent "$need_mb")"; then
    echo "FTW needs about $need_mb MB free to unpack the download. Free space: ${TMPDIR:-/tmp} $(space_of "${TMPDIR:-/tmp}"), /var/tmp $(space_of /var/tmp). Free some space, then run the line again." >&2
    exit 2
  fi
  # A resumed install may have copied most of its release already.
  if [[ "$mode" == --fresh-host ]] && ! has_room /opt "$need_mb"; then
    echo "FTW needs about $need_mb MB free in /opt for its releases, and /opt has $(space_of /opt) free. Free some space, then run the line again." >&2
    exit 2
  fi
  work="$(mktemp -d "$staging/ftw-install.XXXXXX")"
  trap 'rm -rf "$work"' EXIT
  archive="ftw-linux-${arch}.tar.gz"

  if [[ "$tag_given" == no && "$mode" == --resume ]]; then
    sudo_ready
    if as_root test -L "$pending"; then
      echo "The pending native install record is a symlink; refusing to resume." >&2
      exit 2
    fi
    record="$(as_root cat "$pending")"
    record_re='^([^ ]+) [0-9a-f]{64}$'
    if [[ "$record" =~ $record_re ]]; then
      tag="${BASH_REMATCH[1]}"
    fi
    if ! native_tag "$tag"; then
      echo "The pending install record at $pending names no native release; pass --tag." >&2
      exit 2
    fi
    echo "Finishing the install of $tag."
  elif [[ "$tag_given" == no ]]; then
    list_url="https://api.github.com/repos/srcfl/ftw/releases?per_page=100"
    # FTW_RELEASES_URL serves a release list as GitHub does. It exists for
    # tests; leave it unset.
    if [[ -n "${FTW_RELEASES_URL:-}" ]]; then
      list_url="$FTW_RELEASES_URL"
      echo "Using the release list at ${FTW_RELEASES_URL} (for testing)." >&2
    fi
    echo "Looking up the newest FTW release."
    found="$(find_release "$beta" "$archive" "$list_url" "$work")" || exit 1
    IFS=$'\t' read -r tag kind has_package <<< "$found"
    if [[ "$has_package" != yes ]]; then
      echo "$tag has no $archive yet. Its files may still be uploading: try again in a few minutes, or pass --tag with an older release." >&2
      exit 1
    fi
    case "$kind/$beta" in
      stable/*) echo "Newest stable release: $tag." ;;
      beta/yes) echo "Newest beta release: $tag." ;;
      *) echo "Newest release: $tag. It is a beta, as there is no stable release yet." ;;
    esac
  fi

  url="https://github.com/srcfl/ftw/releases/download/${tag}"
  protocols=(--proto '=https' --proto-redir '=https')
  # FTW_RELEASE_MIRROR serves /download/<tag>/ as GitHub does. It exists to
  # test unpublished releases; leave it unset.
  if [[ -n "${FTW_RELEASE_MIRROR:-}" ]]; then
    url="${FTW_RELEASE_MIRROR%/}/download/${tag}"
    protocols=()
    echo "Using the release mirror at ${FTW_RELEASE_MIRROR} (for testing)." >&2
  fi
  echo "Downloading $archive from $tag."
  curl --fail --silent --show-error --location --retry 3 ${protocols[@]+"${protocols[@]}"} \
    "${url}/${archive}" -o "${work}/${archive}"
  curl --fail --silent --show-error --location --retry 3 ${protocols[@]+"${protocols[@]}"} \
    "${url}/${archive}.sha256" -o "${work}/${archive}.sha256"

  checksum="$(cat "${work}/${archive}.sha256")"
  expected="${checksum:0:64}"
  if [[ ! "$expected" =~ ^[0-9a-f]{64}$ ||
        "${checksum:64:2}" != "  " || "${checksum:66}" != "$archive" ]]; then
    echo "The release checksum does not name the expected package." >&2
    exit 1
  fi
  actual="$(sha256sum "${work}/${archive}" | cut -d ' ' -f 1)"
  if [[ "$actual" != "$expected" ]]; then
    echo "The release package checksum does not match." >&2
    exit 1
  fi

  # The package's launcher checks every archive entry, the embedded version,
  # architecture and state schema before it makes a complete release visible.
  tar -xOf "${work}/${archive}" ftw-launcher > "${work}/ftw-launcher"
  chmod 0755 "${work}/ftw-launcher"
  stage="${work}/slot-root"
  mkdir -p "$stage"
  "${work}/ftw-launcher" -root "$stage" install "$tag" \
    "${work}/${archive}" "${work}/${archive}.sha256"
  "${work}/ftw-launcher" -root "$stage" init "$tag"
  "${work}/ftw-launcher" -root "$stage" status >/dev/null

  # No host write occurs until the whole package has passed verification.
  sudo_ready
  if [[ "$mode" == --refresh ]]; then
    for path in "${installed_paths[@]}"; do
      if ! as_root test -f "$path" || as_root test -L "$path"; then
        echo "The native install at $path is missing or a symlink; --refresh changes nothing." >&2
        exit 2
      fi
    done
    release="${stage}/releases/${tag}"
    # The launcher is not running: it replaced itself with Core when it
    # started it, so its file can be swapped in place.
    as_root install -m 0755 -o ftw -g ftw "${work}/ftw-launcher" /opt/ftw/ftw-launcher
    if [[ -f "${release}/ftw-cli" ]]; then
      as_root install -m 0755 -o root -g root "${release}/ftw-cli" /usr/local/bin/ftw
    fi
    if as_root cmp -s "${release}/deploy/ftw-native.service" /etc/systemd/system/ftw.service; then
      unit=unchanged
    else
      as_root install -m 0644 "${release}/deploy/ftw-native.service" /etc/systemd/system/ftw.service
      as_root systemctl daemon-reload
      unit=changed
    fi
    as_root sync
    echo "The launcher and the ftw command now come from $tag. Core itself is unchanged; ftw update moves it."
    if [[ "$unit" == changed ]]; then
      echo "The service definition changed. It applies at the next restart: sudo systemctl restart ftw"
    fi
    exit 0
  fi
  for path in "${existing_paths[@]}"; do
    if as_root test -e "$path" || as_root test -L "$path"; then
      echo "Existing FTW installation found at $path. Keep its data. Use a new card/host or separate Docker setup as described in https://github.com/srcfl/ftw/blob/master/docs/native-beta.md; only one Core may control the equipment." >&2
      exit 2
    fi
  done
  if as_root systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
     as_root systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1; then
    echo "An FTW service is running; refusing to install." >&2
    exit 2
  fi
  if [[ "$mode" == --fresh-host ]] &&
     { as_root systemctl cat ftw.service >/dev/null 2>&1 || id ftw >/dev/null 2>&1; }; then
    echo "An FTW service or account appeared during package verification; refusing to install." >&2
    exit 2
  fi
  if port_busy 8080; then
    echo "Port 8080 is now in use; refusing to install." >&2
    exit 2
  fi
  if [[ "$mode" == --resume ]]; then
    if as_root test -L "$pending"; then
      echo "The pending native install record is a symlink; refusing to resume." >&2
      exit 2
    fi
    if [[ "$(as_root cat "$pending")" != "$tag $expected" ]]; then
      echo "The interrupted install used another tag or checksum; refusing to replace it." >&2
      exit 2
    fi
    if as_root test -L /opt/ftw ||
       { as_root test -e /opt/ftw && ! as_root test -d /opt/ftw; }; then
      echo "The pending native install has an unsafe /opt/ftw path." >&2
      exit 2
    fi
    if as_root test -L /etc/systemd/system/ftw.service; then
      echo "The pending native service is a symlink; refusing to resume." >&2
      exit 2
    fi
    if as_root test -e /etc/systemd/system/ftw.service &&
       ! as_root cmp -s /etc/systemd/system/ftw.service \
         "${stage}/releases/${tag}/deploy/ftw-native.service"; then
      echo "The pending native service differs from the verified package; refusing to replace it." >&2
      exit 2
    fi
    if as_root test -e /etc/systemd/system/forty-two-watts.service ||
       as_root test -L /etc/systemd/system/forty-two-watts.service; then
      alias_target="$(as_root readlink /etc/systemd/system/forty-two-watts.service || true)"
      if [[ "$alias_target" != ftw.service && "$alias_target" != /etc/systemd/system/ftw.service ]]; then
        echo "An unrelated FTW service alias exists; refusing to resume." >&2
        exit 2
      fi
    fi
  else
    printf '%s %s\n' "$tag" "$expected" > "${work}/pending"
    as_root install -m 0600 "${work}/pending" "$pending"
    # --resume needs this record after a power cut during the copy below.
    as_root sync
  fi
  echo "Installing FTW in /opt/ftw."
  if ! id ftw >/dev/null 2>&1; then
    as_root useradd --system --user-group --home-dir /var/lib/ftw --no-create-home ftw
  fi
  as_root install -d -m 0755 /opt/ftw
  as_root cp -a "${stage}/." /opt/ftw/
  as_root install -m 0755 "${work}/ftw-launcher" /opt/ftw/ftw-launcher
  as_root chown -R ftw:ftw /opt/ftw
  as_root /opt/ftw/ftw-launcher -root /opt/ftw status >/dev/null
  # The operator command. Root owns this copy, so the service account cannot
  # replace a program that people may run with sudo.
  operator_command=no
  if as_root test -f "/opt/ftw/releases/${tag}/ftw-cli"; then
    as_root install -m 0755 -o root -g root "/opt/ftw/releases/${tag}/ftw-cli" /usr/local/bin/ftw
    operator_command=yes
  fi
  as_root install -m 0644 \
    "${stage}/releases/${tag}/deploy/ftw-native.service" \
    /etc/systemd/system/ftw.service
  # Flush the release, its receipt, the command and the unit before the service
  # starts. The launcher cannot start or roll back from a file lost to a power cut.
  as_root sync
  as_root systemctl daemon-reload
  echo "Starting FTW."
  if ! as_root systemctl enable --now ftw.service; then
    as_root systemctl disable --now ftw.service || true
    echo "FTW did not start. Inspect journalctl -u ftw, then run the installer again with --resume. Persistent data remains in place." >&2
    exit 1
  fi
  for _ in 1 2 3; do
    sleep 2
    if ! as_root systemctl is-active --quiet ftw.service; then
      as_root systemctl disable --now ftw.service || true
      echo "FTW stopped during startup. Inspect journalctl -u ftw, then run the installer again with --resume. Persistent data remains in place." >&2
      exit 1
    fi
  done
  as_root rm "$pending"

  echo
  echo "FTW $tag is running. To set it up, open one of these from a phone or"
  echo "computer on the same network:"
  setup_addresses /setup
  echo "Check the reported version, storage health and live device readings before use."
  if [[ "$operator_command" == yes ]]; then
    echo "On this machine, ftw status shows its state, ftw update installs a newer release"
    echo "and ftw backup makes a verified backup. Run ftw help for the rest."
  fi
}

# Run unless another script sources this file to test its functions.
# Piped into bash, BASH_SOURCE is empty.
if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
