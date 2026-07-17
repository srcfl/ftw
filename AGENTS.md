# FTW project guide

FTW is a local-first home energy management system written in Go, with Lua
drivers and an optional Python/CVXPY optimizer.

## Architecture

The repository has three explicit modules:

- **Core** — `go/cmd/ftw`, `go/internal` and `web`. Core owns state,
  telemetry, control, safety, API and UI.
- **Drivers** — `drivers/*.lua`, hosted by `go/internal/drivers`. Drivers own
  vendor protocols and are the only place power signs are converted.
- **Optimizer** — `optimizer`, behind the contract in `go/internal/mpc`.
  It proposes plans; core validates them and retains a Go fallback.

Keep new functionality in core unless it has a narrow versioned contract,
independent failure/update semantics and a safe unavailable state. Optional
modules never bypass core safety.

Read [docs/architecture.md](docs/architecture.md) for the system map and
[docs/site-convention.md](docs/site-convention.md) before changing power math.

## Non-negotiable invariants

- Positive W means power into the site; negative W means power out.
- Sign conversion happens only at the driver boundary.
- Planner output is never sent directly to hardware.
- Stale site-meter data stops dispatch.
- A failed/stale driver receives its autonomous default mode.
- Every clamp protects a quantified hardware or control risk.
- Persistent device state is keyed by stable hardware identity, not a YAML name.
- SQLite queries stay in `go/internal/state`.

## Drivers

`go/internal/drivers/lua.go` is the source of truth for the Lua host API.
Every driver implements `driver_init`, `driver_poll`, `driver_command` and a
safe `driver_default_mode`; `driver_cleanup` is optional. Declare catalog
metadata in the driver's `DRIVER` block and request only required capabilities.
Report make and serial as soon as they are known.

Drivers are hot-editable and ship without a compilation step. See
[docs/writing-a-driver.md](docs/writing-a-driver.md).

## Code conventions

- Use `slog` for logging.
- Prefer explicit mutexes and simple ownership over clever atomics.
- Keep packages cohesive; depend on narrow interfaces.
- Put tests beside Go code as `_test.go`.
- Keep full-stack tests in `go/test/e2e`.
- Treat code, types, tests and driver metadata as the detailed documentation.
- Add prose only for architecture, safety invariants or operator steps the code
  cannot explain.

## Build and test

```bash
make test         # Go and Python suites; independent work runs in parallel
make verify       # tests, compose migration, vet and build
make e2e          # full local stack
make dev          # simulators + app
make build-arm64  # Raspberry Pi binary
make release      # local release artifacts only
npm test          # web tests
```

Run the narrow package/test while iterating, then `make verify` before handoff.
Lua drivers have syntax and contract checks in the Go test suite.

## Releases

Changesets drive versioning. Every user-visible code change needs a
`.changeset/*.md` entry; documentation- and CI-only changes are auto-exempt.
Do not edit `package.json` version or `CHANGELOG.md` manually.

Only two release channels exist:

- **beta** receives every new candidate;
- **stable** promotes the exact commit already published and validated as beta.

There is no edge channel. Core and signed driver artifacts can release
independently but follow the same beta-to-stable progression. See
[docs/self-update.md](docs/self-update.md).

## Useful source entry points

| Concern | Source |
|---|---|
| Process wiring and control tick | `go/cmd/ftw/main.go` |
| Configuration schema | `go/internal/config`, `config.example.yaml` |
| HTTP routes | `go/internal/api/api.go` |
| Lua host and registry | `go/internal/drivers` |
| Safety/control | `go/internal/control`, `go/internal/telemetry` |
| Persistence | `go/internal/state` |
| Planner contract/fallback | `go/internal/mpc` |
| Optional optimizer | `optimizer` |
| Driver catalog | `DRIVER` blocks in `drivers/*.lua` |

When behavior looks wrong, inspect the source and its tests before adding a new
document. Keep [docs/](docs/) small and current.
