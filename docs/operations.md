# Operations

The operator's guide. From "I have a fresh Raspberry Pi" to "I can debug a hung driver at 3am".

Sign convention reminder: throughout this doc, `grid_w > 0` means **import** (buying from grid), `grid_w < 0` means **export** (selling). Battery `bat_w > 0` means **charging** (load, draws from grid), `bat_w < 0` means **discharging** (source, reduces import). PV `pv_w` is always ≤ 0 (generation pushes energy into the site). See [site-convention.md](site-convention.md) for the full convention.

## 1. Build + cross-compile

Pure-Go build, no CGO, single static binary:

```bash
# On a dev machine (macOS / Linux), cross-compile for the Pi:
cd /Users/fredde/repositories/forty-two-watts/go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/forty-two-watts-arm64 ./cmd/forty-two-watts

# Sanity check — should report arm64, statically linked
file /tmp/forty-two-watts-arm64
# → ELF 64-bit LSB executable, ARM aarch64, statically linked
```

For amd64: `GOARCH=amd64`.

The Makefile has the usual pair of targets. Prefer these when iterating:

```bash
make build-arm64   # → bin/forty-two-watts-linux-arm64
make build-amd64   # → bin/forty-two-watts-linux-amd64
make release       # → release/forty-two-watts-linux-{arm64,amd64}.tar.gz (bundles binary + WASM + web + config.example.yaml)
```

`make release` bakes in the git tag via `-ldflags "-X main.Version=..."`, so the running binary reports its version in the startup log.

## 2. Files to deploy

The Pi needs:

- **The binary** — single file, no runtime dependencies.
- **`web/`** — static UI assets (`index.html`, `app.js`, `style.css`, `models.js`, `settings.js`, `plan.js`, `twins.js`, `favicon.svg`).
- **`drivers/`** — the Lua driver files (`ferroamp.lua`, `sungrow.lua`, …) if you run the Lua runtime, or **`drivers-wasm/`** for the WASM-based drivers.
- **`config.yaml`** — operator-edited; see [`config.example.yaml`](../config.example.yaml) for the schema and [configuration.md](configuration.md) for every field.

Suggested layout on the Pi:

```
/home/fredde/forty-two-watts-go/
├── forty-two-watts          # binary
├── config.yaml              # local config (don't commit)
├── web/                     # static UI
├── drivers/                 # Lua drivers
├── drivers-wasm/            # WASM drivers (optional)
├── state.db                 # created on first run
├── state.db-wal
├── state.db-shm
├── cold/                    # parquet rolloff (created on first run)
└── forty-two-watts.log      # service log (if redirecting stdout/stderr)
```

The binary only takes two command-line flags:

- `-config config.yaml` — path to the config file.
- `-web web` — path to the static UI directory.

Relative WASM driver paths in `config.yaml` are resolved against the config file's directory, so keep them side-by-side.

## 3. systemd unit

The repo ships [`deploy/forty-two-watts.service`](../deploy/forty-two-watts.service):

```ini
[Unit]
Description=forty-two-watts EMS (Go + WASM port)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=fredde
WorkingDirectory=/home/fredde/forty-two-watts-go
ExecStart=/home/fredde/forty-two-watts-go/forty-two-watts -config config.yaml -web web
Restart=on-failure
RestartSec=5
StandardOutput=append:/home/fredde/forty-two-watts-go/forty-two-watts.log
StandardError=inherit
# Limit memory growth (RPi has 4GB).
MemoryMax=512M

[Install]
WantedBy=multi-user.target
```

Notes:

- `User=fredde` is a **template** — change for your install.
- `WorkingDirectory=/home/fredde/forty-two-watts-go` — this is where `state.db`, `cold/`, and the log file are created, so pick a persistent path.
- `Restart=on-failure` with `RestartSec=5` — the service comes back automatically after a crash.
- `MemoryMax=512M` — Pi-friendly cap. The service typically sits at 50–80 MB resident (see §9).

Install:

```bash
sudo cp deploy/forty-two-watts.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now forty-two-watts
systemctl status forty-two-watts
```

## 4. Service lifecycle

```bash
sudo systemctl start forty-two-watts
sudo systemctl stop forty-two-watts
sudo systemctl restart forty-two-watts
sudo systemctl reload forty-two-watts   # not supported — the unit has no ExecReload; use the hot-reload path instead (§6)
```

For a binary swap with minimum downtime:

```bash
scp /tmp/forty-two-watts-arm64 user@pi:~/forty-two-watts-go/forty-two-watts.new
ssh user@pi 'sudo systemctl stop forty-two-watts && \
  cd ~/forty-two-watts-go && \
  mv forty-two-watts.new forty-two-watts && \
  chmod +x forty-two-watts && \
  sudo systemctl start forty-two-watts'
```

Typical stop → start window: 2–5 seconds. The service records a `shutdown` event on clean stop and a `startup` event on boot, both visible in the `events` table.

## 5. Logs

Because the unit uses `StandardOutput=append:…/forty-two-watts.log`, journald does **not** capture stdout — the log file is authoritative. `StandardError=inherit` means stderr goes to journald.

```bash
# Tail the file log
tail -f /home/fredde/forty-two-watts-go/forty-two-watts.log

# Recent stderr / service lifecycle via journald
journalctl -u forty-two-watts -n 200 --no-pager
journalctl -u forty-two-watts -f
```

forty-two-watts uses `slog` with the text handler at `LevelInfo`. Levels:

- `INFO` — normal operation (startup, reloads, rolloff completions, driver state changes).
- `WARN` — recoverable issues (driver send failure, stale site meter, config reload rejected).
- `ERROR` — fatal or near-fatal (`load config`, `open state`, `http server`). The service either exits or restarts via systemd.

Notable log lines you will look for:

- `forty-two-watts starting version=… config=…` — every boot.
- `config loaded site=… drivers=N` — config parsed OK.
- `HTTP API listening addr=:8080` — UI is reachable.
- `config watcher started path=…` — hot-reload is active.
- `config reload: applied` — a successful `config.yaml` edit was picked up.
- `site meter telemetry stale — idling batteries this cycle` — see §8.

## 6. Hot-reload

Edit `config.yaml` and save. `fsnotify` ([`go/internal/configreload/watcher.go`](../go/internal/configreload/watcher.go)) watches the file's directory, debounces editor saves for 500 ms, then re-parses the file and applies the diff to the running process.

Look for `config reload: applied` in the log. If the new config has a syntax or validation error, the watcher logs `config reload failed err=…` and the service **keeps running the old config** — your site never ends up un-configured because of a typo.

What the watcher can change live (no restart):

- `site.grid_target_w`, `site.grid_tolerance_w`, `site.slew_rate_w`, `site.min_dispatch_interval_s`
- Driver set (add / remove / reconfigure — the registry diffs and respawns)
- Driver capacities (refreshed for dispatch + fuse guard)

Changes that require a full restart (the watcher will not re-wire these):

- `api.port`
- `state.path`, `state.cold_dir`
- HA MQTT bridge parameters
- Planner enable/disable (MPC service is built once at startup)

When in doubt, `sudo systemctl restart forty-two-watts`.

### In-app updates (Docker deploy)

For the `docker-compose.yml` deployment, the web UI's version badge and "Update" / "Restart" buttons drive `docker compose pull` + `up -d` via the `ftw-updater` sidecar. See [self-update.md](self-update.md) for the architecture and how to test the flow locally before a release.

## 7. Backup + restore

Three things hold state:

- **`state.db`** — SQLite (config, battery models, devices, prices, forecasts, recent TSDB tier). Highest priority.
- **`cold/YYYY/MM/DD.parquet`** — long-format TS history for data > 14 days old. Medium priority (losing it loses history, not control).
- **`config.yaml`** — operator-edited config. High priority; may not be reproducible from git.

Backup (stop for a consistent SQLite snapshot):

```bash
# On the Pi
cd ~/forty-two-watts-go
sudo systemctl stop forty-two-watts
tar czf ~/backup-$(date +%F).tgz state.db state.db-wal state.db-shm cold/ config.yaml
sudo systemctl start forty-two-watts
```

Online backup is possible (SQLite is WAL-mode and survives `cp`), but cold backup is the safe default.

Restore: stop the service, extract into `WorkingDirectory`, start the service. On first startup after restore, device-identity reconciliation and battery-model key migration are idempotent — nothing to do manually.

## 8. Troubleshooting runbook

### Driver hung (tick_count not advancing)

**Symptom:** UI shows stale battery values; `/api/status` shows a fixed `tick_count` for a driver across multiple requests.

**Confirm:**

```bash
for i in 1 2 3 4 5; do
  curl -s localhost:8080/api/status | jq '.drivers'
  sleep 2
done
```

The tick should advance every control interval (default 5 s). If it doesn't, the driver VM is wedged.

**Fix:** The watchdog (`site.watchdog_timeout_s`, default 60 s) auto-reverts stuck drivers to their autonomous defaults — you'll see `driver telemetry stale — marking offline + reverting to autonomous` in the log. For a full reset of MQTT/Modbus client state:

```bash
sudo systemctl restart forty-two-watts
```

### MQTT broker connection lost

**Symptom:** Ferroamp telemetry frozen; `MQTT disconnected` or reconnect attempts in the log.

**Confirm:**

```bash
tail -f /home/fredde/forty-two-watts-go/forty-two-watts.log | grep -i mqtt
ping -c 3 192.168.1.X   # your broker IP
```

**Fix:** The paho MQTT client auto-reconnects — a short network hiccup self-heals. If the broker is down, bring it up. If credentials changed, edit `config.yaml` (the `mqtt.*` block) and save — hot-reload will respawn the driver.

### Dispatch not following targets

**Symptom:** Battery doesn't move toward the commanded target.

**Confirm:** compare `dispatch[].target_w` vs `drivers.<name>.bat_w` in `/api/status`:

```bash
curl -s localhost:8080/api/status | jq '{targets: .dispatch, drivers: .drivers}'
```

**Common causes:**

- `power_w` field not threaded through the driver payload (a real regression that was fixed; see commit `9237156`).
- Driver forced into a manual/autonomous mode that conflicts with EMS commands.
- Saturation curve in the battery model too restrictive — check `/api/models` for a pathological gain.
- Sign confusion: remember `bat_w > 0` = charging (load), `bat_w < 0` = discharging (source).

### Ferroamp app shows odd battery power limits

**Symptom:** The Ferroamp app shows values such as `8000 / 8000` under
the battery power command, or the operator has to enter the Ferroamp app
and toggle mode after the EMS has restarted or hot-reloaded.

**Interpretation:** That app view is the EnergyHub's external power
reference / max-min command surface, not the actual battery power. The
actual EMS reading is `drivers.ferroamp.bat_w` in `/api/status` and the
`battery_w` time-series metric.

**Fix:** The Ferroamp MQTT driver publishes `auto` on init, watchdog
fallback, zero battery command, deinit, and cleanup. A clean service
stop or hot-reload should therefore release the EnergyHub back to
autonomous self-consumption. If actual battery power still caps below
hardware capability, set explicit per-driver limits in `config.yaml`;
unset values fall back to the conservative 5 kW dispatcher default:

```yaml
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    battery_capacity_wh: 15200
    max_charge_w: 8000
    max_discharge_w: 8000
```

Then save `config.yaml` and look for `config reload: applied` in the
service log.

### PV prediction wildly off

**Symptom:** `/api/pvmodel` shows huge β coefficients; the twin chart shows e.g. −13 kW for a 10 kW system.

**Fix:** Reset the PV model; the sanity envelope will keep it in range thereafter.

```bash
curl -X POST localhost:8080/api/pvmodel/reset
```

### Battery model orphaned after rename

**Symptom:** You rename a driver in `config.yaml` and the battery model for it appears "lost" in `/api/models`.

**Fix:** It shouldn't actually be lost — models are re-keyed by `device_id`, which resolves the same physical hardware to the same key. Verify:

```bash
curl -s localhost:8080/api/devices | jq
```

If the device_id is endpoint-only (no serial yet) and you changed the IP / endpoint at the same time, the key *will* change and you'll get a cold-start. Restore the old endpoint, or accept re-learning.

### Site meter stale → batteries idling

**Symptom:** `WARN: site meter telemetry stale — idling batteries this cycle` in the log; batteries sit at 0 W regardless of demand.

This is a **safety feature**, not a bug: stale grid readings would otherwise cause one battery to charge another. The control loop refuses to dispatch until the site meter recovers.

**Fix:** Investigate the site meter driver (Ferroamp by default — check `site.meter_driver` in `config.yaml`). Common causes: MQTT broker lost, network partition, Ferroamp rebooted. Restart the service if the driver process itself is wedged.

### Service won't start: port 8080 in use

**Confirm:**

```bash
sudo lsof -i :8080
```

**Fix:** Kill the conflicting process — usually a stale forty-two-watts from a crash that didn't shut down cleanly. `sudo systemctl stop forty-two-watts` first (in case systemd is racing you), then kill the orphan, then `sudo systemctl start forty-two-watts`.

### Config reload rejected

**Symptom:** You edited `config.yaml` and saved, but no `config reload: applied` line appears — just a `config reload failed err=…`.

**Fix:** The old config is still live; the site is safe. Read the error (usually a YAML syntax issue or a validation failure such as "no site meter"), fix, save again. If you want to force a full restart after a big change, `sudo systemctl restart forty-two-watts`.

## 9. Resource expectations

On a Raspberry Pi 4 (4 GB):

| Resource | Typical | Notes |
|---|---|---|
| RAM (RSS) | 50–80 MB | Capped at 512 MB by `MemoryMax` in the unit |
| CPU | < 5% idle | Brief spikes during MPC replan (~600 µs DP solve, every `planner.interval_min`) |
| Disk (recent TSDB) | ~1 GB/month | `state.db` — auto-rolled off to `cold/` after 14 days |
| Disk (cold archive) | ~100–200 MB/year | `cold/YYYY/MM/DD.parquet` — daily files, long-format |

The rolloff loop runs once per hour and is cheap when nothing is due (a single `SELECT` returning 0 rows).

## 10. Where state lives

| File | Owner | Purpose | Backup priority |
|---|---|---|---|
| `state.db` | sqlite (WAL) | Config, models, devices, prices, forecasts, recent TSDB | High |
| `state.db-wal`, `state.db-shm` | sqlite | WAL sidecars — include in backup | High |
| `cold/YYYY/MM/DD.parquet` | rolloffLoop | Long-format TS history > 14 days | Medium |
| `config.yaml` | operator | All operator-tunable settings | High |
| `forty-two-watts.log` | service | Diagnostic log (stdout redirect) | Low |

See [architecture.md](architecture.md) for the full data flow.
