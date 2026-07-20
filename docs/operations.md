# Operations

FTW is normally deployed with Docker Compose on Linux. The core control loop
remains local; the Python optimizer is optional and core falls back safely when
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
sidecar. See [self-update.md](self-update.md).

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
If an older rollback leaves the service offline, follow the Swedish
[failed-rollback recovery procedure](recover-failed-rollback.sv.md) before
changing ownership or deleting any SQLite sidecar files.

## Configuration

`config.example.yaml` and `go/internal/config` define the current schema.
Edits are validated before application. A rejected hot reload leaves the
previous live configuration intact.

Driver set and most control values reload live. Listener addresses, state
paths and some integration transports are startup bindings; restart after
changing them. When unsure, inspect the restart classification in
`go/internal/config/restart_required.go`.

## LAN and API access

FTW accepts state-changing requests without credentials only when they are
addressed through a local name or address: loopback, private/link-local IP,
an unqualified hostname, `.local`, `.localhost`, or `.home.arpa`. The setup
wizard follows the same rule, and the actual client address must also be local.
Browser writes must also be same-origin; FTW checks `Origin`, `Host`, and
`Sec-Fetch-Site` and does not advertise CORS.
Local non-browser clients such as `curl` and Home Assistant may omit browser
fetch headers, but JSON bodies must use `Content-Type: application/json`.
Active reads that start discovery, begin an authorization flow, or force an
external update check pass through the same boundary. Cached and ordinary
read-only requests remain compatible.

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

The built-in browser UI does not store API tokens. For a public/FQDN browser
deployment, put FTW behind an operator-managed HTTPS reverse proxy with login
or session authentication and have that trusted proxy inject the Bearer header
upstream. This token protects mutations, not read-only dashboard data, so FTW
must not be published directly to the internet.

Recovery cannot be disabled by a bad token: connect through `localhost`, the
host's private IP, or its `.local` name, correct/remove `FTW_API_TOKEN`, and
restart Core. Tokens shorter than 32 characters are ignored and remote
mutations remain locked.

`FTW_API_TOKEN` is an operator-managed migration mechanism, not the identity or
tunnel credential for future remote access. That expansion point is described in
[architecture.md](architecture.md#future-remote-access-boundary).

## Logs and health

```bash
docker compose logs --tail=200 ftw
docker compose logs -f ftw ftw-optimizer
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

Inspect `ftw-optimizer` logs and the shared socket volume. Core continues with
the Go fallback; optimizer recovery does not require a core data reset.

### MQTT device missing

Verify the broker address from the same network namespace as core, then inspect
broker and driver logs. Device credentials and topic mappings belong to the
driver configuration.

### Configuration rejected

Read the validation error, compare with `config.example.yaml`, fix the file and
save again. Do not delete `state.db` to resolve a YAML error.

### Port already in use

```bash
sudo ss -ltnp | grep ':8080'
```

Stop the conflicting service or change the configured API port, then restart.

## Native deployment

`make build-arm64` and `make build-amd64` produce static Core and `ftw-backup`
binaries.
`deploy/ftw.service` is the reference systemd unit. A conventional layout is:

```text
/opt/ftw/                 binary, web, bundled drivers, optional optimizer
/etc/ftw/config.yaml      operator configuration
/var/lib/ftw/             state, history, custom/managed drivers
```

Run the binary with `-help` for its current flags. Native installs that omit
Python use the Go planner fallback and normally leave container self-update
disabled.

## Release recovery

Use `stable` for normal sites and `beta` for deliberate validation. A beta
and its promoted stable build identify the same commit. Roll back through the
update UI when a retained snapshot is appropriate; otherwise pin the previous
immutable image tag and restore the matching state backup.
