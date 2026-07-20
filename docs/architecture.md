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
| Drivers | Canonical packages in Device Support; bundled recovery in `drivers/*.lua`; host in `go/internal/drivers` | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
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

Device Support owns canonical Sourceful driver source, SemVer, permissions,
provenance and signed target artifacts. FTW selects only the `ftw-core` target.
Each Lua artifact still contains its own `DRIVER` metadata and implements the
FTW lifecycle. `go/internal/drivers/lua.go` is the source of truth for FTW's
host API and capability sandbox. Network and protocol capabilities must be
granted in configuration.

Drivers are the only hardware-specific layer. They must:

- translate telemetry and commands to the site sign convention;
- report stable make and serial identity when available;
- implement a safe autonomous default mode;
- avoid policy decisions that belong in core;
- remain independently testable and hot-editable.

Bundled drivers provide the offline recovery set. A signed Device Support
index is discovery only; FTW independently verifies the selected package and
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

## Future remote access boundary

Remote Sourceful access is not implemented by the LAN/API hardening layer. The
central request policy in `go/internal/api` is the expansion point: a future
protected remote request must present a locally verifiable principal and access
proof there before an API handler runs.

The machine identity has one compatibility contract regardless of where its
private key lives:

- a gateway name is never operator-selected. It is derived deterministically
  as adjective-color-animal from the stable gateway ID, using exactly the
  existing Sourceful Energy Gateway mapping;
- on compatible hardware, the existing hardware-protected P-256 key and its
  normalized 18-hex serial number are the gateway/site-controller identity.
  The private key never leaves the secure element;
- without that hardware, FTW creates and persists a P-256 identity with the
  same public-key and signature wire contract and a stable ID in the same
  format;
- remote-access policy and composition depend only on the public identity and
  a narrow signing interface. They must not depend on a key path, exported
  private-key bytes, or a particular private-key encoding.

The three-word name is a derived display label and the DNS alias is a rotatable
convenience; neither is an authenticator or secret. Local passkeys authenticate
users; the resolved machine identity authenticates the tunnel endpoint.

The intended authentication and transport remain deliberately separate:

- the resolved, permanent site/machine identity is authoritative for the
  gateway;
- FTW authenticates its tunnel by signing a fresh challenge with that key, not
  with a long-lived bearer or shared tunnel secret;
- FTW initiates one long-lived, outbound-only TLS connection to the relay;
- a small versioned multiplexing contract carries concurrent browser request,
  passkey ceremony, and event streams over that connection;
- passkey authentication terminates on the local FTW instance. FTW stores only
  the WebAuthn verification material needed for enrolled credentials:
  credential ID, public key, counter, and operator-visible name. The private
  passkey never leaves its authenticator, and the relay stores no user
  credential;
- first enrollment starts from the LAN with a single-use, high-entropy QR/link
  secret that FTW validates locally. It does not use a PIN;
- the recommended address is `<alias>.home.sourceful.energy`. The rotatable
  alias is only a convenience; relay state maps it to the public site identity
  and is limited to routing, status, and timestamps.

For this future flow, no password or shared access key is retained by FTW or
the relay. The single-use enrollment secret exists only to authorize the
initial local ceremony and cannot become a standing login credential.

After local passkey verification, FTW issues short-lived access proofs bound to
the site and allowed scopes and verifies them at the central request policy.
Read-only scopes come first. Later mutation scopes must still enter through the
same policy and ordinary Core handlers. Core remains authoritative for
telemetry freshness, validation, clamps, planning, and hardware dispatch; no
remote principal or transport can bypass those checks. Expired or invalid
proofs fail closed, and an unavailable remote connection leaves local control
and local recovery intact. The target has no cloud account authentication,
peer-to-peer or NAT-traversal layer, inbound remote listener, or
browser-managed site directory or secrets.

Before remote implementation, a focused identity ADR must freeze the existing
gateway-name mapping, stable-ID normalization and fallback derivation, P-256
public-key/signature encodings, and identity recovery/rotation rules as
versioned compatibility contracts. This LAN/API change only preserves the
central policy expansion point; it does not choose or implement an identity
provider.

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
3. `go/cmd/ftw/main.go`
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
