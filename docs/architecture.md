# Architecture

FTW is a local-first home energy management system. Its architecture has
three explicit modules: **core**, **drivers**, and **optimizer**. Core is the
safety boundary. Drivers translate hardware protocols. The optimizer proposes
plans. A failure or upgrade outside core must never stop local measurement or
make dispatch unsafe.

## Module boundaries

| Module | Source | Runtime | Responsibility |
|---|---|---|---|
| Core | `go/cmd/ftw`, `go/internal`, `web` | One Go binary | Configuration, telemetry, state, API/UI, safety, control and fallback planning |
| Drivers | `drivers/*.lua`, host in `go/internal/drivers` | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
| Optimizer | `optimizer`, contract in `go/internal/mpc` | Optional Python service/process | Solve the long-horizon mathematical plan |

Core can run without the optimizer. Hardware cannot be accessed without a
driver, but one failed driver is isolated from the others. Optional
integrations such as Home Assistant, CalDAV, notifications and Nova attach at
core's API, state or telemetry boundaries; they do not own dispatch safety.

A future module belongs outside core only when it has:

- a small, explicit and versioned contract;
- independent failure and update semantics;
- no authority to bypass core's validation or safety limits;
- a useful fallback or a cleanly unavailable state.

## Power convention

Above the driver boundary, positive power flows into the site and negative
power flows out. Examples: grid import is positive, PV production is negative,
battery charge is positive and battery discharge is negative.

Only drivers convert vendor signs. Core, storage, API, UI and optimizer all use
the site convention. See [site-convention.md](site-convention.md) before
changing power math.

## Core

`go/cmd/ftw/main.go` is the composition root. It wires configuration, driver
registry, telemetry, persistent state, control, planning, API and integrations.
Packages under `go/internal` should stay cohesive and communicate through
narrow Go interfaces or data types instead of reaching into one another's
storage.

The main flow is:

```text
device
  ↕ vendor protocol
Lua driver                 optional optimizer
  ↕ site-convention data       ↓ proposed trajectory
telemetry → control/planner → core validation and safety → driver command
     ↘ SQLite/history       ↘ API/UI and integrations
```

The in-memory telemetry store owns latest readings and driver health. SQLite
owns durable configuration state, history, forecasts, prices, device identity
and learned model state. Database access stays in `go/internal/state`.

The control loop computes a site target, allocates it across capable assets,
applies safety constraints, then sends commands through the driver registry.
Planner output is an input to that loop, never a direct device command.

## Drivers

Each `drivers/*.lua` file contains its own `DRIVER` metadata and implements the
driver lifecycle. `go/internal/drivers/lua.go` is the source of truth for the
host API and capability sandbox. Network and protocol capabilities must be
granted in configuration.

Drivers are the only hardware-specific layer. They must:

- translate telemetry and commands to the site sign convention;
- report stable make and serial identity when available;
- implement a safe autonomous default mode;
- avoid policy decisions that belong in core;
- remain independently testable and hot-editable.

Bundled drivers provide the recovery set. Signed driver artifacts can be
updated independently through `beta` and `stable`; activation is explicit and
atomic. See [writing-a-driver.md](writing-a-driver.md) and
[device-repository.md](device-repository.md).

## Optimizer

The Python/CVXPY optimizer is optional and separately deployable. Core sends a
versioned planning request and accepts only a complete, valid trajectory. The
optimizer does not read hardware or issue commands.

If the socket/process fails, times out or returns invalid output, core falls
back to its Go planner. Optimizer deployment and dependency churn therefore do
not enlarge the safety-critical runtime.

## Failure boundaries

Core enforces these invariants regardless of mode or module:

- stale site-meter data stops dispatch;
- stale or failed drivers are put in their autonomous default mode;
- configured power, fuse, SoC and slew limits are enforced after planning;
- incomplete or invalid optimizer output is rejected;
- external integrations fail soft and cannot block the control loop;
- persistent writes and activated driver artifacts are atomic.

The concise safety rationale is in [safety.md](safety.md). Tests next to the
relevant code are the detailed executable specification.

## Configuration and interfaces

`config.example.yaml` and the structs plus validation in
`go/internal/config` define the configuration schema. The handlers registered
in `go/internal/api/api.go` define the HTTP surface. Driver metadata defines
the device catalog. These sources replace manually duplicated reference docs.

Some startup bindings cannot be hot-reloaded, including state paths, API
listener and selected integration transports. Normal device and control
configuration is reloaded through `go/internal/configreload`.

## Releases

There are two channels:

- `beta`: every new release candidate, used for real-site validation;
- `stable`: promotion of the exact commit already published and tested as beta.

Core and drivers may release independently, but both use the same progression.
There is no edge channel. See [self-update.md](self-update.md).

## Start reading

1. [site-convention.md](site-convention.md)
2. this document
3. `go/cmd/ftw/main.go`
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
