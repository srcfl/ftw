# Architecture

FTW is a local-first home energy management system. Its architecture has
three explicit modules: **core**, **drivers**, and **optimizer**. Core is the
safety boundary. Drivers translate hardware protocols. The optimizer proposes
plans. A failure or upgrade outside core must never stop local measurement or
make dispatch unsafe.

## Module boundaries

| Module | Source | Runtime | Responsibility |
|---|---|---|---|
| Core | [`go/cmd/ftw`](../go/cmd/ftw), [`go/internal`](../go/internal), [`web`](../web) | One Go binary | Configuration, telemetry, state, API/UI, safety, control and fallback planning |
| Drivers | Editable source in [`srcfl/device-drivers`](https://github.com/srcfl/device-drivers); bundled recovery in `drivers/*.lua`; host in [`go/internal/drivers`](../go/internal/drivers) | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
| Optimizer | [`optimizer`](../optimizer), contract in [`go/internal/mpc`](../go/internal/mpc) | Optional Python service/process | Solve the long-horizon mathematical plan |

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

[`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go) is the composition root. It wires
configuration, driver registry, telemetry, persistent state, control, planning,
API and integrations.
Packages under [`go/internal`](../go/internal) should stay cohesive and communicate through
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
and learned model state. Database access stays in
[`go/internal/state`](../go/internal/state).

The control loop computes a site target, allocates it across capable assets,
applies safety constraints, then sends commands through the driver registry.
Planner output is an input to that loop, never a direct device command.

## Drivers

The public `srcfl/device-drivers` repo owns editable driver source, versions,
contracts, tests and FTW's signed release channel. FTW downloads only an
explicitly selected, content-addressed Lua asset after it verifies the signed
manifest. It never runs raw code from the repository branch. Device Support
may later consume an exact public commit for other products or a higher support
level.

Each Lua artifact still contains its own `DRIVER` metadata and implements the
FTW lifecycle. [`go/internal/drivers/lua.go`](../go/internal/drivers/lua.go) is
the source of truth for FTW's
host API and capability sandbox. Network and protocol capabilities must be
granted in configuration.

Drivers are the only hardware-specific layer. They must:

- translate telemetry and commands to the site sign convention;
- report stable make and serial identity when available;
- implement a safe autonomous default mode;
- avoid policy decisions that belong in core;
- remain independently testable and hot-editable.

Bundled drivers provide the offline recovery set. A signed distribution index
is discovery only; FTW independently verifies the selected package and
artifact, while activation remains explicit and atomic. See
[writing-a-driver.md](writing-a-driver.md) and
[device-repository.md](device-repository.md).

## Optimizer

The Python/CVXPY optimizer is optional and separately deployable. Core sends a
versioned planning request and accepts only a complete, valid trajectory. The
optimizer does not read hardware or issue commands.

If the socket/process fails, times out or returns invalid output, core falls
back to its Go planner. Optimizer deployment and dependency churn therefore do
not enlarge the safety-critical runtime.

## Versioning a module contract

Drivers and the optimizer release on their own schedules, so core cannot assume
the version on the other side of either contract. Both use the same rule.

Each side declares the **window** of contract versions it speaks — core in
[`go/internal/components`](../go/internal/components) and
[`go/internal/optimizercontract`](../go/internal/optimizercontract), a driver in its
`host_api_min`/`host_api_max` metadata, the optimizer in its handshake reply.
An overlap of one version is enough. Declaring a single version means a window
of one.

Grow the contract by adding **features** to the handshake, not by bumping the
version. A feature an old peer does not advertise costs nothing — core simply
does not ask it for what it cannot do — while a version bump makes every peer
outside the new window incompatible at once. That is the mistake the `champion`
requirement made: it landed in core before any optimizer image advertised it,
and every site that had not updated the optimizer silently fell back to the Go
planner.

When the framing or the request shape genuinely changes, bump the version and
**widen** the window rather than moving it, so sites that have not updated the
module keep working.

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

[`config.example.yaml`](../config.example.yaml) and the structs plus validation
in [`go/internal/config`](../go/internal/config) define the configuration
schema. The handlers registered in
[`go/internal/api/api.go`](../go/internal/api/api.go) define the HTTP surface. Driver metadata defines
the device catalog. These sources replace manually duplicated reference docs.

Some startup bindings cannot be hot-reloaded, including state paths, API
listener and selected integration transports. Normal device and control
configuration is reloaded through
[`go/internal/configreload`](../go/internal/configreload).

## Remote access boundary

Remote access is the FTW app and nothing else. The box holds one outbound WSS
connection to `wss://relay.ftw.energy`; there is no inbound listener, no
NAT-traversal layer, no cloud account and no browser-managed site directory.
See [ADR 0006](adr/0006-app-uplink.md) for why Home Link was removed rather
than kept alongside it.

The path is four packages:

- [`go/internal/appenroll`](../go/internal/appenroll) — the Noise static key,
  the rotatable rendezvous secret, the single-use pairing code, the QR payload
  and the list of app keys that have been let in. All of it is boot-time
  material stored beside `nova.key`, not in SQLite;
- [`go/internal/appwire`](../go/internal/appwire) — frames, the Noise IK
  responder and the transport;
- [`go/internal/appproto`](../go/internal/appproto) — the message layer;
- [`go/internal/appuplink`](../go/internal/appuplink) — the outbound
  connection, the per-epoch rendezvous handle and session demultiplexing.

The properties that matter:

- the app's trust anchor arrives optically. The QR payload is a URL fragment,
  which is never sent in an HTTP request, so a hostile or compelled
  `app.ftw.energy` can deny service but cannot impersonate a box;
- the box is not a WebAuthn relying party and stores no user credential. It
  authorises a first connection by the pairing code inside the Noise handshake
  and later ones by the app's pinned static key;
- the relay forwards encrypted frames and holds no keys. The handle the box
  joins under is derived per epoch from the rendezvous secret, so it rotates
  hourly and gives the relay operator nothing stable to key a household on;
- the machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
  is separate and does not authenticate this connection. It resolves to a
  hardware-protected P-256 key where the hardware exists and a bound software
  key with the same public-key and signature wire contract otherwise, with a
  deterministic adjective-color-animal display name derived from the stable
  18-hex gateway ID. The name is a label, never an authenticator;
- authority is unchanged by the transport. A command carries an expiry and
  preconditions, and core revalidates against fresh state before acting. Core
  remains authoritative for telemetry freshness, validation, clamps, planning
  and dispatch. An unavailable relay leaves local control and local recovery
  intact.

## Releases

There are two channels:

- `beta`: every new release candidate, used for real-site validation;
- `stable`: promotion of the exact commit already published and tested as beta.

Core, Optimizer and signed Drivers may release independently, but all use the
same beta-to-stable progression. Core and its privileged updater remain a
paired control plane; optional components negotiate compatibility with Core.
There is no edge channel. See [self-update.md](self-update.md).

## Start reading

1. [site-convention.md](site-convention.md)
2. this document
3. [`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go)
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
