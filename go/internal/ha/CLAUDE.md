# ha — Home Assistant MQTT bridge (autodiscovery + state + commands)

## What it does

One-shot autodiscovery publisher + periodic state pump + command subscriber, all on a dedicated paho MQTT client separate from the per-driver clients in `../mqtt`. On connect it registers site-level sensors (grid/pv/battery/load/SoC/grid_target), a `select` for mode, a `number` for grid target, and four per-driver sensors (`meter_w`, `pv_w`, `bat_w`, `bat_soc_pct`); then it publishes values every `PublishIntervalS` seconds and listens for four command topics. The site sign convention from `docs/site-convention.md` is honored throughout — HA charts drop in without flipping signs.

## Key types

| Type | Purpose |
|---|---|
| `Bridge` | The running service (paho client, pub loop, diagnostics counters). |
| `CommandCallbacks` | Callbacks into the control layer: `SetMode / SetGridTarget / SetPeakLimit / SetEVCharging` (`bridge.go:28`). |

## Public API surface

- `Start(cfg, tel, ctrl, ctrlMu, driverNames, cb) (*Bridge, error)` — connects, publishes discovery, starts the publish loop.
- `(*Bridge).Reload(newCfg, driverNames) error` — tears down the current paho client + publish loop, swaps `cfg`/`driverNames` under `mu`, and re-connects. Diagnostic counters reset because the new connection is its own thing — operators reading `LastPublishMs` / `SensorsAnnounced` after a reload should see "fresh connection" semantics. Errors if the bridge has been Stop'd (lifecycle is one-shot — the configreload applier resurrects HA by calling `ha.Start` instead, see `cmd/forty-two-watts/main.go`).
- `(*Bridge).Stop()` — closes the stop channel, waits for the loop, disconnects with 500 ms quiesce. Idempotent (`stopped` flag).
- `(*Bridge).IsConnected() bool` / `BrokerAddr() string` / `LastPublishMs() int64` / `SensorsAnnounced() int` — diagnostics consumed by `/api/ha/status` in `../api`.

## How it talks to neighbors

Reads `../telemetry.Store` via `Get(driver, DerMeter/PV/Battery)` and `ReadingsByType` to aggregate per-driver smoothed values, then publishes under `forty-two-watts/state/<field>` for site aggregates and `forty-two-watts/driver/<name>/<field>` for per-driver rows (`bridge.go:357-361`). Discovery messages go under `homeassistant/sensor|select|number/forty_two_watts/…/config` (retained). Commands arrive on `forty-two-watts/cmd/{mode, grid_target_w, peak_limit_w, ev_charging_w}` and are forwarded to the injected `CommandCallbacks`; all callbacks run under the caller-supplied `ctrlMu` semantics. See `docs/ha-integration.md`.

## What to read first

1. `bridge.go:105-139` — `Start` wires the paho options, the `OnConnect` re-publishes discovery on every reconnect (HA de-dupes by `unique_id`, so it's safe).
2. `bridge.go:163-244` — `publishDiscovery` is the schema — add a new HA entity here.
3. `bridge.go:248-278` — `subscribeCommands` is the inbound routing table.
4. `bridge.go:299-353` — `publishState` is where aggregation happens; this is what moves every 5 s.

## What NOT to do

- **Do NOT share the paho client with `../mqtt` or a driver.** The bridge uses `clientID = "forty-two-watts-ha"` and sets its own `OnConnectHandler` that re-publishes discovery — that's all wrong for a driver. Keep it isolated.
- **Do NOT block in a subscribe callback.** The callbacks in `subscribeCommands` must return fast; if a callback needs heavy work, dispatch it onto a goroutine (current callbacks are atomic control-state setters, which is fine).
- **Do NOT drop the site sign convention when adding a sensor.** `publishState` pulls `SmoothedW` values that are already signed per the convention; HA's `device_class: power` expects those signs. Flipping here would confuse every existing dashboard.
- **Do NOT change `deviceID` or `topicPrefix` casually.** HA devices and retained discovery topics are keyed on them; a rename orphans every existing HA entity. Migrate with intention.
- **Do NOT bypass `lifecycleMu` from inside Reload / Stop.** The applier in `cmd/forty-two-watts/main.go` can fire Reload twice in rapid succession (debounced editor save followed by an API POST, say). `lifecycleMu` is what prevents the second tick from interleaving its `teardown()` with the first one's `connectAndStart()`. Touching it requires keeping that ordering invariant.
- **Do NOT add QoS > 0 for telemetry.** Publish at QoS 0; the 5 s cadence makes QoS 1 storage-of-unacked messages an unnecessary memory hazard on long reconnects. Retained discovery is the exception (line 189).
