# SolarEdge PV curtail + operator manual hold — design

Date: 2026-05-23
Status: approved, ready to implement

## Why

The forty-two-watts MPC already annotates `PVLimitW` on plan slots where
exporting PV at negative or zero price would lose money, and the
`control.ComputePVCurtail` dispatcher already allocates that limit across
drivers marked `supports_pv_curtail: true`. Eleven hybrid/battery drivers
already implement the `curtail` / `curtail_disable` actions in lua. The
ten PV-only inverter drivers do not.

This change adds curtail support to the SolarEdge drivers and gives the
operator a UI affordance to install a manual PV cap from the dashboard,
mirroring the existing battery manual hold — primarily so a fresh
deploy can be verified against live hardware without waiting for the
MPC to organically trigger a negative-price slot.

Out of scope: a new "negative price optimization" config toggle, an
always-on live-price gate that fires outside planner modes, broadening
the MPC's curtail trigger, and curtail support for any inverter other
than SolarEdge.

## Driver layer

Touch `drivers/solaredge.lua` and `drivers/solaredge_pv.lua`.
`drivers/solaredge_legacy.lua` is out of scope (pre-SunSpec register
map).

Implement `driver_command(action, power_w, cmd)`:

- `"curtail"` with `power_w`: convert to centi-percent of nominal
  (`pct = clamp(power_w / nominal_w * 10000, 0, 10000)`), then write
  SunSpec Model 123 holding registers:
  - `WMaxLimPct` ← pct
  - `WMaxLimPct_RvrtTms` ← 120  (failsafe: cap auto-clears 120 s after
    last refresh, so process death can't permanently throttle the site)
  - `WMaxLim_Ena` ← 1
  Each subsequent curtail tick rewrites the same registers, keeping the
  120 s watchdog timer rolling.
- `"curtail_disable"` or `"deinit"`: write `WMaxLim_Ena = 0`.

Discover Model 123's base register address by walking the SunSpec model
chain once at init starting from 40069 (id + length pairs). Cache the
address. Read nominal `WRtg` from Model 120 (nameplate) once at init,
cache. Fall back to a `nominal_w` config field if Model 120 isn't
present on the inverter firmware.

Add `"pv-curtail"` to the driver `capabilities` table so the catalog
advertises it. Header comment documents the SetApp precondition
("Limit Control Mode = Export Control / Production"). Both drivers stay
`verification_status = "experimental"` until verified on real hardware.

## Manual hold — backend

Sibling of the existing battery manual hold (`api_battery_manual.go`,
`control.BatteryManualHold`).

`control` package adds:

```go
type PVManualHold struct {
    Driver    string    // "" = site-aggregate
    LimitW    float64   // 0..site_pv_capacity
    ExpiresAt time.Time
}

func (s *State) SetPVManualHold(h PVManualHold)
func (s *State) ClearPVManualHold()
func (s *State) GetPVManualHold(now time.Time) (PVManualHold, bool)
```

`ComputePVCurtail` consults the hold *before* the planner-derived
`PVLimitW`. If a hold is active and not expired:

- Driver `""` → use the hold's `LimitW` as the site-wide cap and run the
  existing proportional allocation across drivers in `SupportsPVCurtail`.
- Driver set → cap applies only to that driver; other curtail-capable
  drivers stay uncapped (released if they were capped previously).

Hold expiry falls back cleanly to whatever the planner directive says,
including no curtail (which translates to `curtail_disable` via the
existing one-shot release).

`api` package adds `api_pv_manual.go` with three handlers and the
matching routes wired in `routes()`:

- `POST /api/pv/manual_hold` — body `{ driver?: string, limit_w?:
  number, limit_pct?: number, hold_s: number }`. Exactly one of
  `limit_w` / `limit_pct` required. When `limit_pct` is sent, the
  handler converts to W using either the current live `|PV|` for the
  driver (driver-scoped hold) or the live `|PV|` sum across all
  curtail-capable PV drivers (site-aggregate hold), so the UI can offer
  both knobs interchangeably. `hold_s` capped at 30 min.
- `DELETE /api/pv/manual_hold` — clears.
- `GET /api/pv/manual_hold` — returns the active hold or `{active:
  false}`.

## Manual hold — UI

New component `web/components/ftw-pv-control.js`. Cloned structure
from `ftw-battery-control.js`. Modal contents:

- Two linked sliders: Watts and Percent (link via live PV reading +
  nominal where known).
- Hold duration: 1 m / 5 m / 15 m / 30 m chips.
- Hold / Release buttons.
- Status line: shows current PV in W, current hold (if any), remaining
  time, source ("operator hold" vs "planner").

While open, polls `/api/pv/manual_hold` every 5 s like the battery
modal does.

`next-app.js` extension: the existing `ftw-planet-click` handler grows
a `role === "pv"` branch that opens the modal, passing `d.id` through
so an expanded per-driver bubble scopes the hold to that one driver
(`""` = aggregate, set when clicking the merged bubble).

The modal queries `/api/drivers/catalog` on first open; if no PV driver
advertises `pv-curtail`, the modal renders an explanatory message
instead of the controls (the bubble itself remains clickable so the
discovery path is obvious).

## Tests

- `go/internal/drivers/solaredge_test.go` (new) — mock the modbus
  capability, dispatch `curtail` at 0 / nominal/2 / nominal, assert
  Model 123 register writes match expected pct + ena=1. Dispatch
  `curtail_disable`, assert `ena=0`.
- `go/internal/control/pv_curtail_test.go` (extend) — case where
  `PVManualHold` overrides the planner-derived `PVLimitW`; case where
  the hold expires and the planner directive takes over; driver-scoped
  hold leaves other PV drivers uncapped.
- `go/internal/api/api_pv_manual_test.go` (new) — validation: missing
  both W and pct → 400; both → 400; hold_s out of range → 400;
  unknown driver → 400; happy paths for site-aggregate and
  driver-scoped POST + GET + DELETE.

## Caveats to surface in driver comments

1. SetApp setting **Limit Control Mode = Export Control / Production**
   must be enabled on the inverter, or writes silently no-op.
2. SunSpec Model 123 reads/writes use FC `holding` to match the
   existing register access in `solaredge.lua`.
3. `WMaxLimPct_RvrtTms = 120 s` means an operator can't be permanently
   throttled if the daemon dies — cap auto-clears.
4. `verification_status = "experimental"` until live-hardware
   verification on the Pi.
