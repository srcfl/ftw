# FTW project guide

FTW is a local-first home energy management system written in Go, with Lua
drivers and an optional Python/CVXPY optimizer.

## Architecture

The repository has three explicit modules:

- **Core** — [`go/cmd/ftw`](go/cmd/ftw), [`go/internal`](go/internal) and
  [`web`](web). Core owns state, telemetry, control, safety, API and UI.
- **Drivers** — `drivers/*.lua`, hosted by
  [`go/internal/drivers`](go/internal/drivers). Drivers own
  vendor protocols and are the only place power signs are converted. This tree
  is a generated snapshot of
  [`srcfl/device-drivers`](https://github.com/srcfl/device-drivers), pinned in
  [`drivers/BUNDLED_SOURCE.json`](drivers/BUNDLED_SOURCE.json); change a driver
  there, not here.
- **Optimizer** — [`optimizer`](optimizer), behind the contract in
  [`go/internal/mpc`](go/internal/mpc).
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
- SQLite queries stay in [`go/internal/state`](go/internal/state).

## Drivers

[`go/internal/drivers/lua.go`](go/internal/drivers/lua.go) is the source of
truth for the Lua host API.
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
- Keep full-stack tests in [`go/test/e2e`](go/test/e2e).
- Treat code, types, tests and driver metadata as the detailed documentation.
- Add prose only for architecture, safety invariants or operator steps the code
  cannot explain.

## Working alongside other people

Several people run agents against this repository at the same time. These
rules exist because each one has already been broken once, and the cost
landed on somebody else's finished work.

- **Read a pull request's reviews before judging it.** Never recommend
  closing, superseding or reworking an open PR without reading what the
  reviewers actually wrote. "A review exists" is not the same as knowing
  what it concluded.
- **Check for open PRs touching the files you are about to change.** If one
  exists, it has right of way: rebase onto it, or say so and pick different
  work. Do not rewrite files out from under an open branch.
- **"This already exists" is a claim that needs evidence of the right kind.**
  For behaviour, that means a test or a run. For anything visual, it means
  rendering both versions and comparing them — counting tokens, selectors
  or symbols in the source does not establish that a UI change landed.
- **Prefer small PRs in one area.** Merging often only works when changes do
  not overlap; a large cross-cutting pass conflicts with everything and ages
  badly while it waits.
- **Respect `.github/CODEOWNERS`.** An owner reviews what lands in their
  area, whoever — or whatever — wrote it.
- **Review web/UI changes in a browser.** A human must inspect the rendered
  interface; AI review and reading the source code are not enough.

Planning documents, design specs, task breakdowns and agent scratch notes
stay out of the repository; [`.github/check-no-planning-docs.sh`](.github/check-no-planning-docs.sh) enforces
this. Commit the change, its tests and a changeset; put the reasoning in the
PR description, where it is read during review and then archived.

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
[`.changeset/*.md`](.changeset/) entry; documentation- and CI-only changes are
auto-exempt. Do not edit [`package.json`](package.json) version or
[`CHANGELOG.md`](CHANGELOG.md) manually.

Only two release channels exist:

- **beta** receives every new candidate;
- **stable** promotes the exact commit already published and validated as beta.

There is no edge channel. Core and signed driver artifacts can release
independently but follow the same beta-to-stable progression. See
[docs/self-update.md](docs/self-update.md).

## Useful source entry points

| Concern | Source |
|---|---|
| Process wiring and control tick | [`go/cmd/ftw/main.go`](go/cmd/ftw/main.go) |
| Configuration schema | [`go/internal/config`](go/internal/config), [`config.example.yaml`](config.example.yaml) |
| HTTP routes | [`go/internal/api/api.go`](go/internal/api/api.go) |
| Lua host and registry | [`go/internal/drivers`](go/internal/drivers) |
| Safety/control | [`go/internal/control`](go/internal/control), [`go/internal/telemetry`](go/internal/telemetry) |
| Persistence | [`go/internal/state`](go/internal/state) |
| Planner contract/fallback | [`go/internal/mpc`](go/internal/mpc) |
| Optional optimizer | [`optimizer`](optimizer) |
| Driver catalog | `DRIVER` blocks in `drivers/*.lua` |

When behavior looks wrong, inspect the source and its tests before adding a new
document. Keep [docs/](docs/) small and current.
