# MyUplink heat-pump telemetry — Step 1 (read-only) design

**Date:** 2026-06-15
**Status:** design, awaiting review
**Workstream:** heating / thermal systems (holistic — see roadmap below)
**Related PR:** #484 `feat/myuplink-driver` (hannesb90) — to be scaled down

## Motivation

We want home heating (heat pumps, thermal stores) under the EMS. The first
contributor attempt (PR #484) modelled a heat pump's thermal stores as *fake
batteries* so the MPC could "discharge" them (block the compressor) during
expensive hours. That approach is clever but wrong-shaped:

- Heat pumps are **deferrable/blockable thermal loads**, not electrochemical
  batteries. Forcing them through the battery abstraction (`emit("battery")`,
  `soc=1.0` pinned, capacity-as-one-shot) confuses every layer that reasons
  about batteries.
- It introduced two ship-blocking bugs in the shared control path (see
  Appendix A) — including breaking charging for every existing battery.

Decision (Fredrik, 2026-06-15): take heating as its **own holistic
workstream**, and sequence it **telemetry-first**. Get real heat-pump data
flowing and observable before designing any thermal-store model or control.

> First principles in play: *Robust over feature-rich — works 100% before
> adding features*; *one integration at a time*; *physics before code*.

## Roadmap (the "helhetsgrepp")

| Step | Scope | Status |
|---|---|---|
| **1** | **Read-only MyUplink telemetry** — observe only | **this spec** |
| 2 | Thermal-store model (what state, what units, COP context) | deferred |
| 3 | Control primitive — block / shed / shift the compressor | deferred |
| 4 | MPC / dispatch integration as a *thermal load*, not a battery | deferred |

Steps 2–4 get their own spec once Step 1 has produced real telemetry to
ground the model.

## Step 1 — design

### Goal

A read-only Lua driver that authenticates to MyUplink Cloud REST v2 and emits
heat-pump scalars into the long-format TS DB. **No control, no DER
abstraction, no Go-side dispatch/MPC/battery-model changes.** It cannot
actuate anything, so it cannot cause harm.

### The driver (`drivers/myuplink.lua`, scaled down from #484)

Reuse hannesb90's OAuth + API work; strip everything else.

**Lifecycle:**
- `driver_init(config)` — read `client_id` / `client_secret` / optional
  `device_id` from config; `host.set_make("MyUplink")`; OAuth
  client_credentials token via `host.http_get` against `/oauth/token`
  (URL-encoded body — kept from his review fix); auto-detect `device_id` via
  `/v2/systems/me` if not configured; `host.set_sn(device_id)`.
- `driver_poll()` — `ensure_auth()`, then `GET /v2/devices/{id}/points` for the
  metric parameter set; emit each via `host.emit_metric`; return 60 000 ms.
- `driver_command` / `driver_default_mode` — **no-ops** (read-only; the
  watchdog has nothing to release).
- `driver_cleanup` — clear token.

**Emitted metrics (NIBE default parameter IDs, overridable via config):**

| Metric | Source param | Notes |
|---|---|---|
| `hp_power_w` | 10012 compressor power | energy-relevant signal |
| `hp_hw_top_temp_c` | 40013 BT6 | °C×10 decode |
| `hp_indoor_temp_c` | 40033 BT50 | |
| `hp_outdoor_temp_c` | 40004 BT1 | |

(More signals — supply/return/brine, COP inputs — can be added later without
schema change; that's the point of `emit_metric`.)

**Capability:** `http` with `allowed_hosts: ["api.myuplink.com"]`. **GET only —
no `http_patch`.**

**Identity:** `set_make("MyUplink")` + `set_sn(device_id)` →
`device_id = "myuplink:<id>"`. One physical pump = one device entry (no
`-hotwater` / `-heating` split, since there's no per-store control yet).

**Secrets:** declare `config_secrets = { "client_secret" }` in the `DRIVER`
table so the API masks it on GET and the UI renders it via the existing
`.drv-secrets-slot` (empty input + saved/missing badge, never in the DOM).

### UI (minimal, optional)

A small "API credentials" fieldset in Settings → Devices for OAuth2
client_credentials drivers: `client_id` (plain text) + `client_secret` via the
`config_secrets` secrets-slot. **Drop the block-charge checkbox** (no control
in Step 1). If we'd rather defer UI entirely, the driver is still configurable
by hand-editing `config.yaml`; the fieldset is a usability nicety, not a
requirement.

### Explicitly excluded from Step 1 (deferred to later steps)

- `emit("battery")` / fake-battery / `soc=1.0`
- `driver_command` block/release control
- `host.http_patch` capability (the whole Go `lua.go` change + its tests)
- `block_charge` config field + `driverLimitsFrom` / `dispatch.go` /
  `mpcBatteryFleetFromConfig` changes
- the block-charge UI checkbox

→ Because Step 1 touches none of those Go files, **both regressions in
Appendix A disappear by construction.**

### Testing

- Lua driver: e2e-style test (or a focused driver test) with a fake MyUplink
  HTTP server returning a token + a `/points` payload; assert the expected
  `hp_*` metrics land in the telemetry store. Mirror the existing
  `lua_http_test.go` rig.
- Config/secrets: assert `client_secret` is masked on `GET /api/config` and
  preserved on POST when the masked placeholder is sent back (the existing
  `maskDriverConfigSecrets` / `restoreDriverConfigSecrets` path — add a case
  if myuplink isn't covered).
- No new Go control/dispatch tests needed (no control code in Step 1).

### Delivery

Scale down PR #484 on hannesb90's branch (`maintainer_can_modify=true`):
push a commit that strips to read-only, plus a friendly English PR comment
explaining the telemetry-first sequencing, crediting his OAuth/API work, and
linking this spec. The control/MPC ideas are recorded as the deferred roadmap
steps, not rejected.

### Changeset

Read-only new driver = **minor** ("new driver / expanded device support").

## Appendix A — why the control path is parked (bugs found in #484)

1. **Charging regression.** `driverLimitsFrom` (main.go) used
   `blockCharge := d.MaxChargeW == 0 && d.BatteryCapacityWh > 0`. Because
   `MaxChargeW` is a `float64` with `omitempty`, an absent field and an
   explicit 0 are indistinguishable. Every existing battery that relies on the
   documented `MaxCommandW` default (ferroamp, sungrow — including Fredrik's
   own site) would flip to `blockCharge=true` → `chargeCap()` returns 0 →
   **can never charge.** Contradicts the #145 contract documented in
   `control/CLAUDE.md` and `dispatch.go`. A correct fix needs an *explicit*
   signal (a dedicated field), not overloading 0.
2. **MPC ignores driver-level `max_charge_w`.** `mpcBatteryFleetFromConfig`
   reads only `cfg.Batteries[name]` (the `*float64` section), never
   `config.Driver.MaxChargeW`. So a fake battery without a `batteries:` entry
   gets `chg = cap/2` (0.5C) and the **MPC plans to pre-heat the thermal
   store** — exactly what the design forbade. The "never charge" guarantee did
   not hold at the planner level, and was untested.

These are real and worth fixing *when* we build Step 3/4 control — with an
explicit `block_charge` (or a thermal-load-specific) primitive, propagated to
both the reactive dispatch and the MPC fleet, plus regression tests for both.
