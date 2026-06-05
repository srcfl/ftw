# Testing guide

How to run the forty-two-watts test suite, where tests live, and what's covered
vs what isn't. Everything below can be pasted into a terminal.

## 1. Test status (snapshot)

| Layer | Status | Run with |
|---|---|---|
| Unit | All green | `cd go && go test ./...` |
| Integration (per-package) | All green | (same — colocated with unit) |
| End-to-end (full stack) | All green | `cd go && go test -timeout 180s ./test/e2e -v` |
| Drivers (Lua) | All green | `cd go && go test ./internal/drivers/` |
| Code coverage | Not tracked yet (manual `go test ./... -cover`) | — |
| Local CI | Configured | `make ci` |
| Pi UI smoke | Configured | `make ci-hw-pi` |

Run the full suite:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go test -timeout 120s -count=1 ./...
```

The suite finishes in under a minute on a modern laptop. The e2e package
(`go/test/e2e`) is the slowest single package (~24s) because it spins up a
real MQTT broker and Modbus TCP server in-process.

For the repo-native local CI, including browser smoke and linux/arm64 build:

```bash
cd /Users/fredde/repositories/forty-two-watts
make ci
```

For the Raspberry Pi candidate slot that serves the new UI on a separate
port and read-only proxies live API data:

```bash
make ci-hw-pi
```

See [`local-ci.md`](local-ci.md) for the safety model, environment
variables, and artifact locations.

## 2. Test layout

| Package | Test file(s) | Coverage area |
|---|---|---|
| `internal/battery` | `model_test.go` | ARX(1) + RLS, cascade, saturation curves |
| `internal/config` | `config_test.go` | YAML parsing + validation |
| `internal/control` | `control_test.go` | PI, dispatch modes, anti-windup, slew, fuse guard |
| `internal/currency` | `currency_test.go` | ECB FX rates |
| `internal/drivers` | `lua_test.go`, `runtime_test.go`, `ferroamp_integration_test.go`, `sungrow_integration_test.go` | Lua runtime, driver lifecycle, host capabilities, command round-trip |
| `internal/forecast` | `forecast_test.go` | met.no + OpenWeather parsing |
| `internal/libcheck` | `smoke_test.go` | Smoke tests for SQLite, YAML, fsnotify, mochi MQTT + paho, modbus, wazero |
| `internal/loadmodel` | `model_test.go` | Hour-of-week buckets + heating coefficient |
| `internal/mpc` | `mpc_test.go`, `stress_test.go`, `reactive_test.go` | DP optimizer, annual savings projection, reactive replan |
| `internal/priceforecast` | `forecast_test.go` | Bayesian-blend price forecaster |
| `internal/prices` | `prices_test.go` | elprisetjustnu / ENTSOE fetcher |
| `internal/pvmodel` | `model_test.go` | RLS solar twin, sanity envelope |
| `internal/selftune` | `selftune_test.go` | Step-response state machine |
| `internal/state` | `store_test.go` | SQLite persistence, tiered history |
| `internal/sunpos` | `sunpos_test.go` | Solar position + POA irradiance |
| `internal/telemetry` | `telemetry_test.go` | Kalman smoothing, watchdog scan |
| `test/e2e` | `stack_test.go` | Full-stack: sims + main + drivers + HTTP API |
| `cmd/sim-ferroamp` | `e2e_test.go` (5 tests) | MQTT pub/sub, charge/discharge, command round-trip |
| `cmd/sim-ferroamp/ferroamp` | `physics_test.go` (11 tests) | Battery physics, SoC clamping, grid balance |
| `cmd/sim-sungrow` | `e2e_test.go` (8 tests) | Modbus reads, command writes, physics response |
| `cmd/sim-sungrow/sungrow` | `sungrow_test.go` (13 tests) | Register encoding, bank writes, mode transitions |

Most runtime packages have direct `_test.go` coverage. `internal/arp` is the
notable package without a direct test file in this snapshot; API, configreload,
HA, MQTT, and Modbus also get full-stack exercise through `test/e2e/stack_test.go`.

## 3. Simulators

forty-two-watts ships two simulators that fake real hardware so you can develop
without a Raspberry Pi or an actual inverter on your bench.

### sim-ferroamp (`go/cmd/sim-ferroamp/`)

- Embedded mochi-mqtt broker on `:1883` (flag: `-addr`)
- Publishes realistic `extapi/data/{ehub,eso,sso}` topics every 1s (flag: `-tick`)
- Subscribes to `extapi/control/request` for `charge`, `discharge`, `auto`, `pplim` commands
- First-order battery response (configurable τ)
- Configurable initial SoC (`-soc`), capacity (`-capacity-wh`), PV peak (`-pv-peak`)

Run standalone:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go run ./cmd/sim-ferroamp
```

### sim-sungrow (`go/cmd/sim-sungrow/`)

- Modbus TCP server on `:5502` (flag: `-addr`, default `tcp://0.0.0.0:5502`)
- Serves SH-series register map with live physics simulation
- Accepts charge/discharge commands via holding registers 13049–13051
- 500ms physics refresh (flag: `-tick`)

Run standalone:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go run ./cmd/sim-sungrow
```

Debug a running sim-sungrow with modpoll:

```bash
modpoll -m tcp -a 1 -r 13019 -c 4 -p 5502 localhost
```

## 4. Running the full stack locally

```bash
# Terminal 1: start both simulators
cd /Users/fredde/repositories/forty-two-watts
make run-sim

# Terminal 2: main app against config.local.yaml
make dev
# → http://localhost:8080
```

`make dev` starts the two Go simulators and the main app. Lua drivers are loaded
from `drivers/` at runtime and do not need a separate build step.

## 5. End-to-end test

`go/test/e2e/stack_test.go` is the integration contract. It spins up:

- Embedded MQTT broker + Ferroamp simulator
- Modbus TCP server + Sungrow simulator
- Full Go control loop + HTTP API on a random port

And verifies:

- `/api/health` responds with status ok and two drivers alive
- `/api/status` fuses telemetry from both drivers correctly (PV negative per site convention)
- PI controller responds to a positive step in `grid_target_w` (batteries charge)
- Target-following mode reverses on a negative step (batteries move toward discharge)
- Mode switches persist via `/api/mode`
- Battery models accumulate samples from the control loop
- `/api/battery_models/reset` clears sample count
- `/api/history` endpoint doesn't error
- Static file serving sets cache headers

Run it directly:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go test -timeout 180s -v ./test/e2e
```

Or via Make:

```bash
cd /Users/fredde/repositories/forty-two-watts
make e2e
```

Note: the e2e stack uses the Lua drivers from `drivers/` directly. There is no
separate driver build prerequisite for `make e2e`.

## 6. Where to add tests for a new feature

| Feature kind | Where to add |
|---|---|
| New PI/control behavior | `go/internal/control/control_test.go` |
| New battery model algorithm | `go/internal/battery/model_test.go` |
| New ML twin behavior | colocated `_test.go` in `pvmodel`/`loadmodel`/`priceforecast` |
| New driver (Lua) | new `*_integration_test.go` in `go/internal/drivers/` (mock the protocol) |
| New API endpoint | add or extend a colocated `_test.go` in `go/internal/api/` |
| Cross-package contract | `go/test/e2e/stack_test.go` |
| Simulator physics | `go/cmd/sim-*/…/*_test.go` (colocated with the physics package) |

## 7. Mocking patterns

Examples drawn from the existing tests:

- **MQTT**: use mochi's embedded broker. See the imports in
  `go/test/e2e/stack_test.go` (`github.com/mochi-mqtt/server/v2`) and the
  integration test in `go/internal/drivers/ferroamp_integration_test.go`.
- **Modbus TCP**: use `github.com/simonvetter/modbus` server-side, or feed the
  sungrow package's in-memory bank directly. Both patterns appear in
  `go/test/e2e/stack_test.go`.
- **Time-dependent code**: pass `time.Time` explicitly; never call `time.Now()`
  inside the function under test. A recent bug in `prices_test.go` was exactly
  this — the date was hard-coded and broke at the calendar boundary; the fix
  is commit `7025cf8`.

## 8. Coverage roadmap

Coverage is not tracked today. To measure manually:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go test ./... -coverprofile=cover.out
go tool cover -func=cover.out | tail -1
go tool cover -html=cover.out
```

Recommended next steps (acknowledged TODOs):

- Add coverage reporting to the existing GitHub Actions test workflow
- Broaden `go/internal/api/` coverage as new endpoints are added
- Add direct tests for `go/internal/arp/`
- Add a regression test for the `lua.go` `power_w` field bug (commit `9237156`)
- Add a regression test for the `LoadAllBatteryModels` deadlock (commit `c387c62`)

## 9. Lua driver tests

The Lua VM lifecycle is tested in `go/internal/drivers/lua_test.go`:

- `TestLuaDriverLifecycle` — parses a stub driver, runs init/poll/command/cleanup
- `TestLuaDriverMissingFile` — error path when the `.lua` file is absent
- `TestLuaDriverSyntaxError` — error path when the driver fails to parse

To smoke-test a new driver file:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go test -count=1 -run TestLuaDriverLifecycle ./internal/drivers/
```

Additional Lua driver patterns live in
`go/internal/drivers/ferroamp_integration_test.go` and
`go/internal/drivers/sungrow_integration_test.go`, which exercise the full
host-API (`host.emit`, `host.modbus_*`, `host.mqtt_*`) round-trip against the
simulators.

## 10. Common test failures

- **Lua driver file not found** — check `-drivers`, `-user-drivers`, or the
  driver path in config. The bundled drivers live under `drivers/` in the repo
  and `/app/drivers` in the container image.
- **e2e timeout** — bump the `-timeout` flag; the e2e test spins up a real
  broker + Modbus server and is slow on CI VMs. Default Make target uses `180s`.
- **Date-dependent test fails** — should be fixed; if you see a new one, follow
  the `prices_test.go` pattern (use `time.Now()` and compute the URL fragment
  dynamically, or inject the clock).
- **Port already in use (`:1883` or `:5502`)** — a previous `make run-sim` left
  a simulator running. `pkill -f sim-ferroamp` / `pkill -f sim-sungrow` and
  retry.
