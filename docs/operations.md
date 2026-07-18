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

Back up the entire directory. For a simple fully consistent backup:

```bash
cd ~/ftw
docker compose stop ftw
sudo tar -C . -czf "ftw-backup-$(date +%F).tgz" data
docker compose start ftw
```

To restore, stop core, replace `data/` from a backup, restore ownership and
start core. Keep the current directory aside until the restored service and
devices are healthy.

The in-app updater also retains bounded pre-update snapshots. These protect
configuration and SQLite state; keep an external backup for host or disk loss.
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

`make build-arm64` and `make build-amd64` produce static core binaries.
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
