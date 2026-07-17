# Development

The repository is split into core, drivers and optimizer. See
[architecture.md](architecture.md) before moving responsibilities between
them.

## Fast loop

```bash
make dev          # simulators + core, creates config.local.yaml when missing
make test         # Go and Python suites
make e2e          # explicit full-stack simulator test
npm test          # web tests
make verify       # fast pre-commit verification
make ci           # e2e, builds and browser smoke
```

`make test` runs independent Go and optimizer work concurrently and reuses the
optimizer virtual environment. Prefer a narrow package or test while iterating,
then run `make verify`. Use `make ci` for a complete local handoff pass.

Run core alone:

```bash
cd go
go run ./cmd/ftw -config ../config.local.yaml -web ../web
```

The UI listens on the configured API port, normally 8080.

## Live-data UI work

`FTW_PROXY_UPSTREAM` forwards local `/api/*` requests to a live LAN instance
while serving UI files from the worktree. Writes are blocked by default.

```bash
FTW_PROXY_UPSTREAM=http://192.168.1.20:8080 \
  go run ./go/cmd/ftw -config config.local.yaml -web web
```

Set `FTW_PROXY_READONLY=0` only for an intentional live write session.

## Containers

`docker compose up -d` mirrors the Linux production topology: core, optimizer,
updater and MQTT broker. Use `docker-compose.macos.yml` on macOS. Local Compose
overrides are machine-specific and untracked.

## Generated files

`bin/`, `dist/`, `artifacts/`, local databases, caches, `node_modules/`
and `optimizer/.venv/` are disposable and ignored. Do not treat generated
output or agent plans as project documentation.
