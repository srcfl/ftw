# Development

The repository is split into core, drivers and optimizer. See
[architecture.md](architecture.md) before moving responsibilities between
them.

## Fast loop

```bash
make dev          # Ferroamp + Sungrow simulators and core; creates config.local.yaml when missing
make sim-ocpp     # Evify OCPP charge points against a running FTW
make test         # Go suites
make e2e          # explicit full-stack simulator test
npm test          # web tests
make verify       # pre-commit: tests, script checks, Energyplan bundle check, vet and build
make ci           # Go suites, e2e, builds and browser smoke
```

Prefer a narrow package or test while iterating, then run `make verify`. Use
`make ci` for a complete local handoff pass. `make verify` and `make release`
also need Python 3 for the release scripts and the bundle verifier.

Run core alone:

```bash
cd go
go run ./cmd/ftw -config ../config.local.yaml -web ../web
```

The UI listens on the configured API port, normally 8080.

## Optimizer

The optimizer is the compiled Energyplan worker checked in at
`optimizer/native/bundle`; see [its README](../optimizer/native/README.md).
There is no toolchain or virtual environment to set up. Development builds plan with Core DP unless
`planner.engine: energyplan` is set; Core then runs the worker for this host
from that directory. `make native-solver-test` verifies the bundle and runs
the worker tests when the host has a bundled worker.

## Live-data UI work

`FTW_PROXY_UPSTREAM` forwards local `/api/*` requests to a live LAN instance
while serving UI files from the worktree. Writes are blocked by default.

```bash
FTW_PROXY_UPSTREAM=http://192.168.1.20:8080 \
  go run ./go/cmd/ftw -config config.local.yaml -web web
```

Set `FTW_PROXY_READONLY=0` only for an intentional live write session.

## Containers

[`deploy/docker`](../deploy/docker) builds a local 0.x image from a published
release package; see [native-beta.md](native-beta.md). The root
`docker-compose.yml` and `docker-compose.macos.yml` (Core, `ftw-updater`
sidecar and Mosquitto) belong to the frozen 1.x–3.x Docker line. Local Compose
overrides are machine-specific and untracked.

## Generated files

`bin/`, `release/`, `artifacts/`, `dev-data/`, local databases, caches and
`node_modules/` are disposable and ignored. Do not treat generated output or
agent plans as project documentation.

`drivers/*.lua` is ignored too: it is a snapshot of the commit pinned in
[`drivers/BUNDLED_SOURCE.json`](../drivers/BUNDLED_SOURCE.json), fetched, never
authored here. A fresh clone or `git worktree` therefore starts without it,
and the first `make test` fetches it; this needs `curl`, `jq` and network
access. `make drivers` fetches it again. Later runs cost nothing.
