# api — HTTP REST + static UI server on :8080

## What it does

Single `http.ServeMux` that serves JSON endpoints for every operator-facing concern (health, status, config, mode, drivers, battery models, self-tune, history, prices, forecast, MPC, PV/load twins, series, devices, HA status) plus fall-through static file serving for the web UI. Uses Go 1.22+ method-scoped routing (`"GET /api/foo"`) so GET + POST can live on the same path. No WebSockets yet — clients poll.

## Key types

| Type | Purpose |
|---|---|
| `Deps` | Container of pointers to every subsystem the handlers touch (`api.go:40`). |
| `Server` | Owns `*http.ServeMux` + `*Deps`; returned by `New(deps)`. |

`Deps` fields include `Tel *telemetry.Store`, `Ctrl *control.State` + `CtrlMu`, `State *state.Store`, `Capacities` + `CapMu`, `Cfg` + `CfgMu`, `Models` + `ModelsMu`, `SelfTune`, `Prices`, `Forecast`, `MPC`, `PVModel`, `LoadModel`, `HA`, `SaveConfig` (injected for testability), `WebDir`, `Version`. Nothing is auto-wired — `main.go` constructs and passes.

## Public API surface

- `New(deps *Deps) *Server`
- `(*Server).Handler() http.Handler` — for `http.ListenAndServe`.
- `(*Server).handle(methodPath, handler)` is internal; see `api.go:134`.

Endpoint groups (route table is `api.go:95-130`):

| Group | Routes |
|---|---|
| status | `GET /api/health`, `GET /api/status` |
| control | `GET|POST /api/mode`, `POST /api/target`, `POST /api/peak_limit`, `POST /api/ev_charging` |
| config | `GET|POST /api/config` |
| drivers | `GET /api/drivers`, `GET /api/drivers/catalog` |
| devices | `GET /api/devices` |
| battery_models | `GET /api/battery_models`, `POST /api/battery_models/reset` |
| self_tune | `POST /api/self_tune/start`, `GET /api/self_tune/status`, `POST /api/self_tune/cancel` |
| history / series | `GET /api/history`, `GET /api/series`, `GET /api/series/catalog`, `GET /api/energy/daily` |
| prices / forecast | `GET /api/prices`, `GET /api/forecast` |
| mpc | `GET /api/mpc/plan`, `POST /api/mpc/replan`, `GET /api/mpc/diagnose` |
| loadpoints | `GET /api/loadpoints`, `POST /api/loadpoints/{id}/target` |
| twins | `GET /api/pvmodel`, `POST /api/pvmodel/reset`, `GET /api/loadmodel`, `POST /api/loadmodel/reset` |
| ha | `GET /api/ha/status` |
| version (self-update) | `GET /api/version/check`, `POST /api/version/channel\|skip\|unskip\|update\|restart`, `GET /api/version/update/status` |
| static | `/` falls through to `Deps.WebDir` |

## How it talks to neighbors

Pure consumer. Every read goes through the corresponding package's public getters (`telemetry.Store.Get / AllHealth / ReadingsByType`, `state.Store.LoadHistory / LoadSeries / AllDevices`, `control.State.Mode / LastTargets`, `ha.Bridge.IsConnected / LastPublishMs`, …). Every write takes the package's mutex before mutating (`CtrlMu` for control state, `CapMu` for capacities, `ModelsMu` for models, `CfgMu` for config). `handlePostConfig` writes via the injected `SaveConfig` so tests can stub disk I/O.

## What to read first

1. `api.go:40-74` — the `Deps` struct documents the whole dependency graph.
2. `api.go:95-130` — `routes()` is the complete route table.
3. `api.go:134-140` — `handle()` shows how Go 1.22 method-scoped routing is used.
4. `api.go:144-159` — `writeJSON` / `readJSON` are the only response helpers; 1 MB body cap on inputs.
5. Any specific handler — most are 10-30 LOC of "take mutex, copy state, release, render".

## What NOT to do

- **Do NOT mutate state without the matching mutex.** Every handler that touches `Deps.Ctrl`, `Deps.Capacities`, `Deps.Models`, or `Deps.Cfg` must take the paired mutex and release it before writing the response.
- **Do NOT add a route outside `routes()`.** Keeping the table in one place is the only way to see the whole surface. If you need dynamic registration, that's a design smell.
- **Do NOT add new handlers to `api.go`.** That file is already long; growing it by one more feature is how it becomes unreviewable. Each new feature (or logically-grouped cluster of endpoints) lives in its own `api_<feature>.go` file — handlers, request/response structs, validation, helpers. `api.go` only gets the one-line route registration in `routes()`. When `api.go` grows a new handler you *must* extract it; when an existing feature in `api.go` gains a second handler, that's the signal to split it into `api_<feature>.go` first. Pattern to follow: `api_selfupdate.go`, `api_loadpoint_policy.go`. Tests mirror the split: `api_<feature>_test.go`.
- **Do NOT read unbounded request bodies.** `readJSON` caps at 1 MB (`api.go:153`); preserve that for new handlers.
- **Do NOT couple a handler to a concrete implementation the other packages didn't expose.** If you reach for an unexported field you'll break `main.go`'s wiring. Extend the subsystem's public API first.
- **Do NOT forget `Deps.SaveConfig`.** Config-writing handlers call it (not `os.WriteFile` directly) so tests can inject an in-memory fake.
- **Do NOT add CORS logic per-handler.** `writeJSON` already sets `Access-Control-Allow-Origin: *` (`api.go:146`); duplicate headers confuse browsers.
