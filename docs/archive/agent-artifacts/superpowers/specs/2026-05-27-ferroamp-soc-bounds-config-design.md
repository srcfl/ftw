# Ferroamp per-driver SoC bounds via YAML config

**Date:** 2026-05-27
**Status:** Approved (verbal, brainstorming session)
**Scope:** `drivers/ferroamp.lua` only. Single-driver, single-file change.

## Problem

The Ferroamp Lua driver hard-codes two SoC thresholds that gate its
multi-ESO dispatch scaling:

```lua
-- drivers/ferroamp.lua
local DISCHARGE_FLOOR_SOC = 0.15
local CHARGE_CEIL_SOC     = 0.95
```

An ESO whose live SoC is above `CHARGE_CEIL_SOC` is excluded from the
`n_charge_capable` count, which makes the on-wire setpoint scale to
zero — effectively a stop-charging gate at 95 %. Same logic on the
discharge side at 15 %.

This is intentional design (LFP cell-balancing zone, headroom for
unexpected PV surplus), but it is bound to a hardcoded constant and
cannot be tuned without forking the driver. Today's operator wants
the option to charge all the way to 100 %; tomorrow's operator may
want something else.

## Solution

Read both thresholds from the driver's YAML `config:` block in
`driver_init`. Default to the current values when unset, so existing
configurations behave identically.

### YAML surface

```yaml
- name: ferroamp
  lua: drivers/ferroamp.lua
  is_site_meter: true
  config:
    charge_ceil_soc: 1.0        # let ESOs charge to 100% (default 0.95)
    discharge_floor_soc: 0.05   # allow discharge to 5%  (default 0.15)
```

Both fields are optional. Unset → existing defaults preserved.

### Lua implementation sketch

```lua
local DISCHARGE_FLOOR_SOC = 0.15
local CHARGE_CEIL_SOC     = 0.95

function driver_init(config)
    host.set_make("Ferroamp")
    ...
    -- existing skip_battery + eso_capacity_kwh handling unchanged ...

    if config and config.charge_ceil_soc ~= nil then
        local v = tonumber(config.charge_ceil_soc)
        if v and v > 0 and v <= 1.0 then
            CHARGE_CEIL_SOC = v
            host.log("info", string.format(
                "Ferroamp: CHARGE_CEIL_SOC = %.3f (from config)", v))
        else
            host.log("warn", string.format(
                "Ferroamp: charge_ceil_soc=%s ignored (must be 0 < v <= 1)",
                tostring(config.charge_ceil_soc)))
        end
    end

    if config and config.discharge_floor_soc ~= nil then
        local v = tonumber(config.discharge_floor_soc)
        if v and v >= 0 and v < 1.0 then
            DISCHARGE_FLOOR_SOC = v
            host.log("info", string.format(
                "Ferroamp: DISCHARGE_FLOOR_SOC = %.3f (from config)", v))
        else
            host.log("warn", string.format(
                "Ferroamp: discharge_floor_soc=%s ignored (must be 0 <= v < 1)",
                tostring(config.discharge_floor_soc)))
        end
    end

    if CHARGE_CEIL_SOC <= DISCHARGE_FLOOR_SOC then
        host.log("warn", string.format(
            "Ferroamp: CHARGE_CEIL_SOC (%.3f) <= DISCHARGE_FLOOR_SOC (%.3f) — usable window is empty",
            CHARGE_CEIL_SOC, DISCHARGE_FLOOR_SOC))
    end
    ...
end
```

Lua's chunk-scoped `local` lets `driver_init` reassign the
file-scope locals, so the rest of the driver picks up the override
without further plumbing.

## What is NOT in scope

- **No Go-side changes.** The generic `Config map[string]any` already
  flows from YAML through `Driver.Init(ctx, config)` into the Lua
  table — no new struct fields, no validation hooks.
- **No generalisation to other drivers.** Sungrow and other inverters
  use entirely different gating mechanisms (e.g. force-charge modbus
  commands that bypass the inverter's own SoC limit register).
  Generalise when we have a second driver that wants the same knob.
- **No coupling to `planner.soc_max_pct`.** The planner cap and the
  driver cap are two independent layers. Operators who want
  100 % charging in practice need to raise both, but that's a
  documentation concern, not a code concern.
- **Not a fix for `liveCurtailLimitW` over-counting headroom.** That
  is a separate dispatch-side bug (see open notes from the 2026-05-27
  curtail brainstorming session).

## Validation rules

| Field | Type | Range | Behaviour outside range |
|---|---|---|---|
| `charge_ceil_soc` | number | `0 < v <= 1.0` | Logged as warning, default kept |
| `discharge_floor_soc` | number | `0 <= v < 1.0` | Logged as warning, default kept |
| `charge_ceil_soc > discharge_floor_soc` | invariant | — | Warning, but values still applied (operator owns the call) |

Non-numeric values (`tonumber` returns `nil`) fall through to the
warning path. Missing values keep the default — `nil` checked
explicitly so an operator who wants to set `0` for the floor isn't
silently ignored.

## Documentation updates

- `CLAUDE.md` Ferroamp section: short note that both bounds are
  config-tunable.
- `docs/configuration.md`: example block showing the two new fields.
- DRIVER metadata block at the top of `drivers/ferroamp.lua`: add the
  two fields to the catalog so the UI can surface them.

## Risk assessment

| Risk | Mitigation |
|---|---|
| Operator sets `charge_ceil_soc: 1.0` and accelerates LFP wear | Ferroamp's own BMS still protects against overcharge; document the trade-off in `docs/configuration.md`. This is operator's call. |
| Operator sets `discharge_floor_soc: 0` and depletes the battery hard | Same — BMS protects, operator-owned trade-off. |
| Typo / wrong type silently ignored | Explicit `nil` check + warning log per invalid value. |
| Lua chunk-scope rule misunderstood | Existing `SKIP_BATTERY = true` reassignment in the same driver uses the identical pattern — proven to work. |

## Testing

- **Manual on live (192.168.192.40):** push the driver to the Pi
  (drivers hot-reload), set `charge_ceil_soc: 1.0` via
  `POST /api/config`, observe Ferroamp charging past 95 % through
  `/api/status`.
- **Unit:** if a Lua-level test exists for the existing
  `skip_battery` / `eso_capacity_kwh` paths, mirror it for the new
  fields. Otherwise skip — the change is mechanically trivial and
  observable end-to-end on hardware.
- **No e2e changes needed** — the e2e test starts the driver against
  a sim that doesn't depend on these thresholds.

## Rollout

1. Merge PR.
2. Operator adds the field to YAML if they want non-default behavior.
3. No migration — existing configurations stay on `0.95 / 0.15`.

The previous `pv-charge-bonus` PR (#362) is unrelated and can ship
independently.
