# Testing Lua drivers live

Practical runbook for iterating on a driver against the bundled
simulators on your laptop, and against real hardware on a Raspberry
Pi. Every command is pasteable and has been verified against current
`master`.

## 1. Local sim loop

The two bundled simulators stand in for real hardware so you never need
a battery + MQTT broker + Modbus server just to exercise a driver.

### 1.1 Start the sims (no main app)

```bash
make run-sim
```

From `Makefile:95-100` this runs `go run ./cmd/sim-ferroamp` (embedded
MQTT broker on `:1883` plus realistic `extapi/*` traffic) and
`go run ./cmd/sim-sungrow` (Modbus TCP server on `:5502`) in parallel.
Ctrl-C kills both.

Useful flags (`go/cmd/sim-ferroamp/main.go:32-38`,
`go/cmd/sim-sungrow/main.go:27-32`):

```bash
cd go && go run ./cmd/sim-ferroamp -addr :1883 -soc 0.3 -capacity-wh 15200
cd go && go run ./cmd/sim-sungrow  -addr tcp://0.0.0.0:5502 -soc 0.8 -capacity-wh 9600
```

### 1.2 Start sims + main app together

```bash
make dev
```

From `Makefile:102-109` this is `make run-sim` plus
`go run ./cmd/forty-two-watts -config ../config.local.yaml -web ../web`.
The main binary parses `config.local.yaml`, spawns every declared
driver, and serves the HTTP API on `:8080`.

Verify telemetry is flowing:

```bash
curl -s localhost:8080/api/status | jq '{mode, grid_w, pv_w, bat_w, drivers}'
```

Verify your driver is discovered by the catalog (used by the Settings
UI, populated from the `DRIVER={…}` table at the top of each `.lua` file
— see `go/internal/drivers/catalog.go`):

```bash
curl -s localhost:8080/api/drivers/catalog | jq '.entries[].filename'
```

Verify it's actually spawned and ticking:

```bash
curl -s localhost:8080/api/drivers | jq
curl -s localhost:8080/api/status  | jq '.drivers'
```

Both should show your driver by name, with `status: "ok"` and
`tick_count` advancing across successive requests.

### 1.3 Hot-reload

The config watcher (`go/internal/configreload/watcher.go`) watches the
*directory* containing `config.yaml`, debounces 500 ms, and on change
calls `reg.Reload` which diffs each driver's config and restarts only
the ones whose fields changed (`go/internal/drivers/registry.go:301`,
`sameDriverConfig` at `:343`).

What this means for live driver development:

- Editing `config.yaml` (or `config.local.yaml` under `make dev`) fires
  the watcher and restarts affected drivers — you'll see
  `config reload: applied` in the log.
- **Editing a `drivers/*.lua` file does NOT trigger a reload by
  itself** — the watcher only watches the config file. To pick up pure
  Lua edits, touch the driver's config entry (e.g. add and remove a
  space) or restart the service. This is a known sharp edge of the
  current design.

Tail logs under `make dev` (everything goes to stdout; slog text
handler at `slog.LevelInfo` — `go/cmd/forty-two-watts/main.go:53`):

```bash
make dev 2>&1 | tee /tmp/42w.log
tail -f /tmp/42w.log
```

There is no `LOG_LEVEL` env var — the level is hardcoded to
`LevelInfo`. To get `host.log("debug", ...)` output, change the line
above locally or temporarily raise a specific log to `info`.

## 2. Unit + integration tests

From the repo root:

```bash
cd go && go test -count=1 -run TestLua ./internal/drivers/
```

This runs the Lua runtime tests in `go/internal/drivers/lua_test.go`.
Catalog parsing has dedicated tests in the same package.

Full suite:

```bash
cd go && go test ./...
make test        # repo default test target
make e2e         # full-stack end-to-end with both sims
```

Lua drivers have no build step.

## 3. Testing on a Raspberry Pi

### 3.1 Build + tarball

```bash
make build-arm64   # → bin/forty-two-watts-linux-arm64
make release       # → release/forty-two-watts-linux-{arm64,amd64}.tar.gz
```

The tarball bundles the binary, `drivers/`, `web/`, and
`config.example.yaml`.

### 3.2 Push + restart

Assuming the service unit is already installed (see
`docs/operations.md` §3):

```bash
PI=pi@pi.local   # edit to taste

# Ship the binary + bundled assets
rsync -avz release/forty-two-watts-linux-arm64.tar.gz $PI:/tmp/
ssh $PI 'cd ~/forty-two-watts-go && \
  tar xzf /tmp/forty-two-watts-linux-arm64.tar.gz && \
  mv forty-two-watts-linux-arm64 forty-two-watts.new && \
  sudo systemctl stop forty-two-watts && \
  mv forty-two-watts.new forty-two-watts && \
  chmod +x forty-two-watts && \
  sudo systemctl start forty-two-watts'

# Ship Lua drivers separately (not in the tarball)
rsync -avz drivers/ $PI:~/forty-two-watts-go/drivers/
```

### 3.3 Tail logs on the Pi

The shipped unit redirects stdout to a file and stderr to journald
(`docs/operations.md` §5):

```bash
ssh $PI 'tail -f ~/forty-two-watts-go/forty-two-watts.log'
ssh $PI 'journalctl -u forty-two-watts -f'   # stderr + lifecycle
```

### 3.4 Iterate on a driver on-Pi

```bash
# Edit locally, push, and touch the config to force a reload
scp drivers/mydriver.lua $PI:~/forty-two-watts-go/drivers/
ssh $PI 'touch ~/forty-two-watts-go/config.yaml'
```

`touch` bumps the mtime; fsnotify fires; the watcher re-parses and
calls `reg.Reload`. Only drivers whose `config.Driver` fields changed
restart, so to force a restart of *just* your driver, briefly rename
its entry in `config.yaml` and save twice (or restart the service).

## 4. Debugging

### 4.1 Device identity

Verify ARP + make/serial resolution worked (see
`docs/device-identity.md`):

```bash
curl -s localhost:8080/api/devices | jq
```

Each device reports `device_id` (`make:serial` > `mac:…` > `ep:…`),
plus the raw `make`, `serial`, `mac`, `endpoint`, and timestamps. If
your driver's `device_id` is still `ep:<endpoint>` after a few
seconds, your `driver_init` hasn't called `host.set_make` +
`host.set_sn` yet (fix it — persistent state keys off `device_id`).

### 4.2 Per-driver telemetry

There is no `/api/telemetry` endpoint. The routes you want are:

```bash
# Aggregate snapshot — per-driver health + latest readings
curl -s localhost:8080/api/status | jq '.drivers.<driver-name>'

# Long-format TSDB — one metric, one driver, over a range
curl -s 'localhost:8080/api/series?driver=mydriver&metric=battery_w&range=1h&points=600' | jq

# Everything your driver has emitted at least one sample of
curl -s localhost:8080/api/series/catalog | jq
```

`range` accepts `5m | 15m | 1h | 6h | 24h | 3d`
(`go/internal/api/api.go:614-624`).

### 4.3 Driver health

```bash
curl -s localhost:8080/api/health | jq
```

Returns `{status, drivers_ok, drivers_degraded, drivers_offline}`. Per-driver
detail (status, consecutive_errors, tick_count, last_error) lives under
`/api/status` in the `drivers.<name>` block.

### 4.4 MQTT / Modbus wire tracing

For a Ferroamp-style (MQTT) driver, watch the sim broker:

```bash
mosquitto_sub -h localhost -t 'extapi/#' -v
```

For a Sungrow-style (Modbus TCP) driver, poke the sim directly:

```bash
modpoll -m tcp -a 1 -r 13019 -c 4 -p 5502 localhost
```

The canonical debug commands are in the top-of-file comments of each
sim (`go/cmd/sim-ferroamp/main.go:10`,
`go/cmd/sim-sungrow/main.go:8`).

## 5. Writing sim fixtures for a new device

If you need a new simulator:

- Copy `go/cmd/sim-sungrow/` as a template for a Modbus device, or
  `go/cmd/sim-ferroamp/` for an MQTT device.
- Put the physics in a sibling package (see `go/cmd/sim-sungrow/sungrow/`
  / `go/cmd/sim-ferroamp/ferroamp/`).
- Wire it into `make run-sim` in the `Makefile` so `make dev` picks it
  up automatically.

If you only need new register fixtures for an existing sim, edit the
bank inside that sim package — no new binary needed.

## 6. Live production diagnostics

### 6.1 Watchdog

`site.watchdog_timeout_s` (default 60 s) is the oldest-telemetry
threshold; beyond it the driver flips to `StatusOffline` and the
control loop sends `DefaultMode` to make it autonomous
(`go/cmd/forty-two-watts/main.go` control-loop branch +
`tel.WatchdogScan`). A driver stuck at the same `tick_count` across
several seconds has a hung VM — `sudo systemctl restart forty-two-watts`
on the Pi, then fix the root cause.

### 6.2 Self-tune + battery model reset

There is no `/api/selftune/reset/<driver>` route. The real surface is:

```bash
# Clear the learned battery model (all or one)
curl -s -XPOST localhost:8080/api/battery_models/reset \
  -H 'Content-Type: application/json' -d '{"battery":"mydriver"}' | jq
curl -s -XPOST localhost:8080/api/battery_models/reset \
  -H 'Content-Type: application/json' -d '{"all":true}' | jq

# Kick off a 3-minute step-response self-tune for one or more batteries
curl -s -XPOST localhost:8080/api/self_tune/start \
  -H 'Content-Type: application/json' -d '{"batteries":["mydriver"]}' | jq
curl -s         localhost:8080/api/self_tune/status | jq
curl -s -XPOST  localhost:8080/api/self_tune/cancel | jq
```

Routes confirmed at `go/internal/api/api.go:109-113`.

See also: `docs/writing-a-driver.md`, `docs/operations.md`,
`docs/api.md`, `docs/safety.md`.
