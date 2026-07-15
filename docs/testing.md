# Testing Guide

This guide covers the Go, Lua, and Python/CVXPY test workflow for FTW.

`make test` creates the local optimizer venv when needed, runs the CVXPY model
suite, then runs Go tests with the real Go-to-Python worker integration enabled.
All commands are run from the repo root unless noted.

Historical planner qualification uses `ftw-optimizer-backtest`; see
[optimizer.md](optimizer.md#historical-backtest). Export and solve are separate
commands so captured live diagnostics can be retained as an immutable test
input and every solver run after export is offline.

## Main Commands

```bash
make test
make e2e
make ci
make dev
make ci-hw-pi
```

Direct Go equivalents:

```bash
cd go
go test -timeout 120s -count=1 ./...
go test -timeout 180s -count=1 -v ./test/e2e
```

`make ci` runs the repo-native local CI path, including browser smoke and
linux/arm64 build checks. See [`local-ci.md`](local-ci.md) for details.

## Test Layout

| Area | Where |
|---|---|
| Battery ARX/RLS models | `go/internal/battery/*_test.go` |
| Config parsing and validation | `go/internal/config/*_test.go` |
| Control and dispatch | `go/internal/control/*_test.go` |
| Driver runtime and Lua host API | `go/internal/drivers/*_test.go` |
| API handlers | `go/internal/api/*_test.go` |
| State, SQLite, TSDB, rolloff | `go/internal/state/*_test.go` |
| Telemetry and watchdog | `go/internal/telemetry/*_test.go` |
| PV/load/price twins and planner | `go/internal/{pvmodel,loadmodel,priceforecast,mpc}/*_test.go` |
| Simulators | `go/cmd/sim-*/*_test.go` |
| Full stack | `go/test/e2e/stack_test.go` |

Use `rg --files go -g '*_test.go'` when you need the live list.

## Simulators

The repo ships simulator binaries for local development and e2e tests.

### Ferroamp simulator

```bash
cd go
go run ./cmd/sim-ferroamp
```

It embeds an MQTT broker, publishes realistic Ferroamp topics, and accepts
charge/discharge/self-consumption commands.

### Sungrow simulator

```bash
cd go
go run ./cmd/sim-sungrow
```

It exposes a Modbus TCP server with an SH-series register map and accepts
battery control writes.

## Local Full Stack

```bash
make run-sim
make dev
```

`make dev` starts the main app against the local config and serves the UI at
`http://localhost:8080`. Lua drivers do not need a build step.

## End-to-End Test

`go/test/e2e/stack_test.go` starts an in-process stack:

- MQTT broker + Ferroamp simulator
- Modbus TCP server + Sungrow simulator
- main app + HTTP API on a random port

It verifies health, status aggregation, control response, mode persistence,
battery model samples, history, and static file serving.

Run:

```bash
make e2e
```

or:

```bash
cd go
go test -timeout 180s -count=1 -v ./test/e2e
```

## Lua Driver Tests

Fast syntax/lifecycle smoke:

```bash
cd go
go test -count=1 -run TestLuaDriverLifecycle ./internal/drivers/
```

Full driver package:

```bash
cd go
go test -count=1 ./internal/drivers/
```

Driver tests cover lifecycle calls, catalog parsing, capability gates, MQTT,
Modbus, HTTP/WebSocket/TCP host helpers where relevant, and selected driver
regressions.

For live hardware workflows, use
[`testing-drivers-live.md`](testing-drivers-live.md).

## Where to Add Tests

| Change | Add tests in |
|---|---|
| New control or dispatch behavior | `go/internal/control/` |
| New API endpoint | `go/internal/api/` |
| New driver | `go/internal/drivers/` plus live notes in the PR |
| New simulator behavior | `go/cmd/sim-*` |
| Cross-package contract | `go/test/e2e/` |
| Config schema change | `go/internal/config/` |
| Persistence/state behavior | `go/internal/state/` |

Prefer focused package tests first. Add e2e coverage when the behavior is a
user-visible workflow or a cross-module safety contract.

## Coverage

Coverage is measured manually today:

```bash
cd go
go test ./... -coverprofile=cover.out
go tool cover -func=cover.out | tail -1
go tool cover -html=cover.out
```

## Common Failures

- **Port already in use**: a previous simulator or app is still running.
  Stop it and rerun the target.
- **Date-dependent price tests**: inject or compute dates dynamically rather
  than hard-coding calendar fragments.
- **Driver lifecycle failure**: check the `DRIVER` metadata block, Lua syntax,
  and required host call signatures such as `host.log(level, msg)`.
- **Capability error in a driver test**: make sure the test config grants the
  same capability the driver calls (`mqtt`, `modbus`, `http`, `websocket`,
  or `tcp`).
- **e2e timeout**: rerun with a larger `-timeout` and inspect simulator/app
  logs for startup failures.
