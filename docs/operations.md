# Operations

FTW is normally deployed with Docker Compose on Linux. The core control loop
remains local; Energyplan ships with Core and Core falls back safely when
it is unavailable.

## Install

Fresh Raspberry Pi OS, Debian or Ubuntu host:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

The default directory is `~/ftw`; persistent data is under `~/ftw/data`.
Open `http://<host>:8080/setup` on the LAN.

Use `docker-compose.macos.yml` on macOS because Linux host networking is not
available through Docker Desktop. Existing installations must follow
[upgrade-from-legacy.md](upgrade-from-legacy.md), not the fresh installer.

Common commands:

```bash
cd ~/ftw
docker compose ps
docker compose logs -f ftw
docker compose restart ftw
docker compose pull
docker compose up -d
```

The UI updater performs an immutable pull and recreate through the updater
sidecar; it never patches the host OS or Docker engine — that is the
operator's job on a self-managed host, and automatic on the
[Raspberry Pi image](rpi-image.md#host-os-security-updates). See
[self-update.md](self-update.md). A 2.x site moving to 3.x must follow
[upgrade-paired-release.md](upgrade-paired-release.md) instead of orange
Update.

## Persistent state

For the Compose deployment, `data/` contains:

- `config.yaml` — operator configuration;
- `state.db` plus SQLite WAL files — state, history and learned models;
- `cold/` — rolled-off Parquet history;
- custom and managed driver data.

Do not store mutable state inside the container. The data directory must be
writable by container uid 100/gid 101.

Create verified full backups from **FTW Update Center → Full backups**, then
download the `.ftwbak` archive to another computer or disk. The format captures
the entire persistent directory, verifies file hashes and SQLite, and records
the installed component versions. Use the safe restore helper, which retains
the current data and automatically reverts it if the restored service does not
become healthy. See [backup-and-restore.md](backup-and-restore.md).

The updater also retains bounded local pre-update rollback points. They protect
configuration and SQLite state during a Core update but remain on the same
disk; they cannot recover a failed SD card.

Core exposes a read-only storage check at
`GET /api/storage/inventory`. It reports allocated, live and free SQLite pages
for `state.db` and `cache.db`, their WAL/SHM sizes, and free space on the volume
that contains the data directory. It does not expose paths, scan a storage
engine's internal files, change retention, checkpoint WAL, run `VACUUM`, or
delete data. Use `GET /api/backups` and `GET /api/version/snapshots` for the
existing backup and rollback-point size lists.

If an older rollback leaves the service offline, follow the Swedish
[failed-rollback recovery procedure](recover-failed-rollback.sv.md) before
changing ownership or deleting any SQLite sidecar files.

## Configuration

[`config.example.yaml`](../config.example.yaml) and
[`go/internal/config`](../go/internal/config) define the current schema.
Edits are validated before application. A rejected hot reload leaves the
previous live configuration intact.

Driver set and most control values reload live. Listener addresses, state
paths and some integration transports are startup bindings; restart after
changing them. When unsure, inspect the restart classification in
[`go/internal/config/restart_required.go`](../go/internal/config/restart_required.go).

## LAN and API access

FTW accepts state-changing requests without credentials only when they are
addressed through a local name or address: loopback, private/link-local IP,
`.local`, `.localhost`, or `.home.arpa`. The setup
wizard follows the same rule, and the actual client address must also be local.
Browser writes must also be same-origin; FTW checks `Origin`, `Host`, and
`Sec-Fetch-Site` and does not advertise CORS.
The box refuses to be framed: every response includes
`Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`.
Local non-browser clients such as `curl` and Home Assistant may omit browser
fetch headers, but JSON bodies must use `Content-Type: application/json`.
Active reads that start discovery, begin an authorization flow, or force an
external update check pass through the same boundary. So do reads of config,
logs, support dump and report, driver source, system info, research dump, the
app-link device list, driver health, EV charger detail, planner diagnose,
time series, and the fleet-ping payload. Live dashboard reads such as status,
energy, prices, plan, loadpoints and history stay compatible.

Mutation requests addressed through any other hostname or a public IP fail
closed. To expose that API intentionally, generate a random token of at least
32 characters and set `FTW_API_TOKEN`. Compose deployments should store it in
the project `.env` file so updater-driven container recreates retain it:

```dotenv
FTW_API_TOKEN=<random-secret-at-least-32-characters>
```

Keep `.env` readable only by the operator (for example, mode `0600`).

Then recreate Core and send the token as a Bearer credential:

```bash
docker compose up -d ftw
curl -X POST -H "Authorization: Bearer <same-random-secret>" \
  https://ftw.example.net/api/restart
```

The same Bearer is accepted on the LAN when `api.lan_auth` is on. `FTW_API_TOKEN`
and the house password are independent secrets: the token is not hashed as a
password guess, and a mismatch does not lock Settings.

The built-in browser UI does not store API tokens. For a public/FQDN browser
deployment, put FTW behind an operator-managed HTTPS reverse proxy with login
or session authentication and have that trusted proxy inject the Bearer header
upstream. The token, and the same-origin / local-address checks, now also
cover config, logs, support dump and report, driver source and identity,
integration status, notification history and rules, system and storage info,
local repository and snapshot paths, research dump, the app-link device list,
driver health, EV charger detail, planner diagnose, time series, and the
fleet-ping payload. Live dashboard reads (status, energy, prices, plan,
loadpoints, history) stay open if someone publishes the port. Still do not
publish the box directly to the internet.

Recovery cannot be disabled by a bad token: connect through `localhost`, the
host's private IP, or its `.local` name, correct/remove `FTW_API_TOKEN`, and
restart Core. Tokens shorter than 32 characters are ignored and remote
mutations remain locked.

`FTW_API_TOKEN` is an operator-managed migration mechanism, not the identity or
tunnel credential for future remote access. That expansion point is described in
[architecture.md](architecture.md#future-remote-access-boundary).

`api.lan_auth` is off by default. Turn it on from loopback inside the
process. On a Pi with host networking that is
`http://127.0.0.1:8080` Settings → System, or `curl` to `127.0.0.1`.
On Docker Desktop (`docker-compose.macos.yml`) a host curl to localhost
arrives as the bridge gateway, not loopback, so Settings and host curl
get 403. Enable from inside the container:

```bash
docker compose -f docker-compose.macos.yml exec ftw \
  wget -qO- --header='Content-Type: application/json' \
  --post-data='{"enabled":true,"password":"ETT-LANGT-LOSEN"}' \
  http://127.0.0.1:8080/api/auth/password
```

Do not treat that gateway as loopback: Desktop SNAT uses the same peer
for every published-port client, including LAN visitors. A first enable
from another LAN address is refused, so a visitor cannot set the
password. `POST /api/config` cannot flip the flag. When on,
protected LAN routes need the house password or `FTW_API_TOKEN`.
`curl` sends `Authorization: Bearer <house-password>` or
`Authorization: Bearer <FTW_API_TOKEN>`. The browser login form
sets a session cookie (`ftw_lan`, 12 hours). Loopback (`127.0.0.1` / `::1`)
never asks. Live status stays readable without the password; a viewer
caller is minted for those reads. The two secrets do not share a lockout:
retrying the API token cannot lock the household out of Settings.

An owner pairing QR is minted only from loopback or with the house
password. Promoting a paired phone to owner uses the same gate. The
first pairing on an empty box is an owner whatever the code said, so
that mint also stays at the box. A viewer invite still works from the
LAN once an owner exists.

The FTW app and Home Assistant MQTT are unchanged.

Recovery: `curl` to `127.0.0.1`, or set `api.lan_auth: false` in
`config.yaml` and restart Core.

### "remote access to protected API routes is disabled"

The full message is `remote access to protected API routes is disabled;
configure FTW_API_TOKEN or use a local address`. The dashboard still loads —
the message appears when a protected request (saving settings, starting an
update, a scan) is refused. It means the request failed the locality rules
above, and "local" is judged on the request, not on which network the
browser sits on:

1. **The address in the browser's address bar** must be a private or
   loopback IP, `localhost`, or a `.local`/`.localhost`/`.home.arpa` name.
2. **The address the connection arrives from** must be loopback, private, or
   link-local.

Being on the same LAN as the box satisfies neither by itself. The common
ways to trip the boundary from the couch next to the Pi:

- **A plain hostname without a dot** — `http://myhost:8080`. Since v2.2.1
  a no-dot name is deliberately not local (a DNS-rebinding guard), so an
  address that worked before an update stops working. Use the `.local`
  name or the IP instead.
- **A router-issued name with a dot in it** — `pi.lan`, `ftw.fritz.box`,
  `box.home`. Only the suffixes listed above count as local names.
- **Tailscale** — both `100.x.y.z` addresses (CGNAT space) and `*.ts.net`
  names count as remote.
- **IPv6** — when the name resolves to a global IPv6 address, the browser
  connects from a global address and fails the second rule, even with a
  `.local` name in the address bar.
- **A reverse proxy or Home Assistant ingress reached through a public
  URL** — the request arrives carrying the public hostname.

The reliable fix for a browser is to open the UI through the box's private
IPv4 address, for example `http://192.168.1.123:8080`. Setting
`FTW_API_TOKEN` does not change what the built-in UI sends — the token is
only for API clients that attach the `Authorization: Bearer` header, as
described above — and a `?token=...` query parameter in the URL does
nothing.

## Logs and health

```bash
docker compose logs --tail=200 ftw
docker compose logs -f ftw ftw-updater
curl -fsS http://localhost:8080/api/health
```

For a native systemd deployment:

```bash
journalctl -u ftw -n 200 --no-pager
journalctl -u ftw -f
systemctl status ftw
```

Warnings about stale site-meter telemetry are safety actions, not cosmetic
noise. Dispatch remains idle until fresh data returns.

## Troubleshooting

### Driver offline or tick count stopped

Check the driver-specific error and transport connectivity. Core should already
have marked it offline and sent its default mode. Restart only after capturing
the useful log context. If telemetry resumes, watchdog recovery is automatic.

### Batteries do not follow the plan

Check, in order:

1. site-meter freshness and sign;
2. selected mode and current plan freshness;
3. asset SoC/capability;
4. fuse/per-phase saturation;
5. driver health and command errors;
6. optimizer status and fallback messages.

A safety clamp or fallback should be visible in status/logs. Do not raise a
limit until the physical installation and configuration agree.

### Optimizer unavailable

Inspect Core logs and `/api/components` for the Energyplan worker status.
Core uses its Go fallback when needed; recovery needs no data reset.

### MQTT device missing

Verify the broker address from the same network namespace as core, then inspect
broker and driver logs. Device credentials and topic mappings belong to the
driver configuration.

### A device `.local` name does not resolve

With `allow_unverified_local` enabled, FTW resolves `.local` device names
itself because a `CGO_ENABLED=0` Go binary never consults NSS. Without the
option, FTW leaves the name to Go's system DNS path. FTW's own lookup has two
backends, and the log line for a successful lookup says which:

```
resolved host over mDNS host=zap.local addr=192.168.1.42 via=avahi
```

**`via=avahi`** — the host's `avahi-daemon` answered over its socket. This is
preferred when available because avahi already holds a record cache. When NSS
and the socket mount are configured, `getent hosts zap.local` asks the same
daemon, though cache timing can still differ.

**`via=multicast`** — FTW queried the LAN directly. This is what happens when
the avahi socket is not mounted, which is the default, and it needs no host
software at all.

Failures log `mDNS resolution failed`, and the message names both backends when
both were tried.

Either way multicast has to reach the LAN, which the Linux Compose topology
provides through `network_mode: host`. Under `docker-compose.macos.yml` the
container is bridged and multicast does not reach the LAN, so configure devices
by IP there. The direct path sends on every active, non-loopback multicast
interface. It supports IPv4 and IPv6; a link-local IPv6 answer is used only
with its interface zone, and an unscoped link-local answer is discarded.

mDNS has no built-in authentication. Treat a `.local` name as a LAN trust
boundary, reserve names used by control drivers, and use TLS certificate pins
where the driver supports them. Network allowlists still check the configured
host name and port before resolution; they do not prove that an mDNS responder
is the intended device.

That risk is not unique to names, which is why the opt-in below gates the
resolver rather than the connection. A raw IP address is no more an identity on
a LAN than a name is — it can be claimed by ARP, and DHCP can hand it to a
different device with no attacker involved at all. The durable check is the
identity a device reports once connected: make and serial, or its MAC.

So by default FTW does not use *its own* mDNS answer for a `.local` name: the
name goes to the system resolver, exactly as it did before this package
existed, and on platforms that answer `.local` themselves — Home Assistant does,
through Supervisor's DNS service — it simply works. Opt in per driver to let FTW
resolve the name itself, over Avahi or the LAN:

```yaml
capabilities:
  allow_unverified_local: true
  modbus:
    host: inverter.local
    port: 502
```

For the Home Assistant bridge, set `homeassistant.allow_unverified_local: true`
instead. It applies to every transport — HTTP, WebSocket, MQTT, Modbus and raw
TCP — and is per driver, so a name allowlist never becomes a server identity for
another driver. Literal IP addresses and ordinary DNS names are untouched.

Without it, a `.local` dial is not refused; it is handed to the system resolver,
and the log records why FTW's own answer was not used. That matters on a host
where nothing else resolves `.local` — a plain Compose or Raspberry Pi install,
whose `resolv.conf` points at the router — because there the name will simply
not resolve until you opt in.

HTTP and WebSocket requests to `.local` hosts always bypass configured proxies,
so credentials and control payloads stay on the LAN path. Ordinary DNS names
still use the configured proxy. A TLS pin does not bypass the mDNS gate yet.

#### Letting FTW use avahi

Host networking shares ports, not Unix sockets, so avahi has to be bind-mounted
in. `docker-compose.yml` carries the line commented out:

```yaml
    volumes:
      - /run/avahi-daemon:/run/avahi-daemon:ro
```

Mount the *directory*, not the socket file inside it. If the host path is
missing Docker creates it, and an empty directory is harmless — whereas a
directory created where the socket belongs stops `avahi-daemon` from ever
starting. Restarting avahi detaches the mount, so restart FTW after you do.

This is an optimisation, not a requirement: device connectivity is unchanged
without it. It lets `getent hosts zap.local`, `curl` and `wget` check the name
from inside the container.

Under the Home Assistant add-on none of this applies: Supervisor mounts only a
fixed set of named paths, so the socket cannot be provided and FTW always
queries the LAN directly. The add-on runs with `host_network: true`, which is
what makes that work.

### Configuration rejected

Read the validation error, compare with
[`config.example.yaml`](../config.example.yaml), fix the file and
save again. Do not delete `state.db` to resolve a YAML error.

### Port already in use

```bash
sudo ss -ltnp | grep ':8080'
```

Stop the conflicting service or change the configured API port, then restart.

## Native deployment

`make build-arm64` and `make build-amd64` produce static Core and `ftw-backup`
binaries.
[`deploy/ftw.service`](../deploy/ftw.service) is the reference systemd unit. A
conventional layout is:

```text
/opt/ftw/                 binary, web, bundled drivers, optional optimizer
/etc/ftw/config.yaml      operator configuration
/var/lib/ftw/             state, history, custom/managed drivers
```

Run the binary with `-help` for its current flags. Native installs without a supported
Energyplan worker use the Go planner fallback and normally leave container self-update
disabled.

## Release recovery

Use `stable` for normal sites and `beta` for deliberate validation. A beta
and its promoted stable build identify the same commit. Roll back through the
update UI when a retained snapshot is appropriate; otherwise pin the previous
immutable image tag and restore the matching state backup.
