# configreload — fsnotify watcher with debounced reload + per-section diff

## What it does

Watches the *directory* containing `config.yaml` (not the file itself — editor rename/atomic-save breaks file-level watchers), coalesces events with a 500 ms debounce, re-parses via `config.Load`, applies changes to the running `control.State` atomically, and dispatches a `(new, old)` diff to an `Applier` callback so subsystems that need restarting (drivers, prices, MPC, etc.) can decide per-section.

## Key types

| Type | Purpose |
|---|---|
| `Applier` | `func(new, old *config.Config)` — called after a successful reload so the caller can diff each section and decide what to restart (`watcher.go:20`). |
| `Watcher` | Holds the fsnotify handle, the shared config pointer + its `RWMutex`, and the control state + its `Mutex`. |

## Public API surface

- `New(path, cfgMu, cfg, ctrlMu, ctrl, applier) (*Watcher, error)` — returns `nil, err` if fsnotify setup or directory registration fails.
- `(*Watcher).Start()` — spawns the watch goroutine; idempotent against restarts.
- `(*Watcher).Stop()` — closes the stop channel and the fsnotify watcher.

## How it talks to neighbors

Calls `config.Load(path)` on each debounced event; on parse failure the old pointer is left intact (`watcher.go:100-102`) — atomic-switch guarantee. Applies control-layer scalars inline under `ctrlMu` (grid target, tolerance, slew rate, min dispatch interval — `watcher.go:111-123`). Everything else is punted to the `applier` callback wired in `cmd/ftw/main.go`, which is where driver-registry `Reload`, price-service restart, MPC re-config, etc. get triggered per section. No imports of `drivers`, `mpc`, `prices`, `ha` — keeps the watcher generic.

## What to read first

`watcher.go` — the whole package. Look at `loop()` (`watcher.go:70-96`) for the debounce pattern: events reset the 500 ms timer; only when it fires does `reload()` run. Then `reload()` (`watcher.go:98-136`) shows the atomic-switch order: parse first, mutate control state, then swap the `*cfg` pointer, then call `applier`.

## What NOT to do

- **Do NOT watch the file directly.** Editors commonly do write→rename to save atomically; `fsnotify` loses the inode. The package watches `filepath.Dir(path)` and filters by basename (`watcher.go:45-47`, `:82`). Don't "simplify" that.
- **Do NOT skip the debounce.** A single editor save fires Write + Rename + Create in rapid succession; the 500 ms coalesce is what stops a triple-reload. If you need faster reloads, build a manual trigger on top, don't remove the timer.
- **Do NOT apply every section inline.** Only the cheap control-scalar changes live in `reload()`. Driver restarts, price fetcher restarts, MPC re-planning, etc. all belong in the `applier` callback so subsystems can diff what *they* care about and avoid unnecessary restarts.
- **Do NOT leave `*cfg` half-updated.** `reload` takes `cfgMu` for writing and does a full `*cfg = *newCfg` in one statement (`watcher.go:127-129`) — keep that single-assignment pattern so readers under `RLock` always see a consistent snapshot.
- **Do NOT log-and-continue when parsing fails.** `reload` explicitly returns early on `config.Load` error (`watcher.go:100-103`); replacing that with a partial apply would silently drift the running system from disk.
