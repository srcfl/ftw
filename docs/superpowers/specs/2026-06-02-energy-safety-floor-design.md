# Forecast-risk reserve via downside PV (replaces the SoC safety floor)

**Date:** 2026-06-02
**Status:** approved (Alt 2), implemented on `feature/pv-downside-safety`
**Area:** `go/internal/mpc`, `go/internal/pvmodel`, `go/internal/config`, `go/cmd/forty-two-watts/main.go`

## Problem

The MPC carried a `soc_safety_floor_pct` (default **25 %**): a soft DP cost
penalty when SoC ended a PV-surplus slot below the floor %, to stop the planner
running the battery to empty in the morning betting on midday PV that clouds
might cancel. Three problems surfaced (field reports, xorath + Fredrik):

1. **Wrong unit.** A percentage is relative to battery size — 25 % of 5 kWh
   (1.25 kWh) vs 25 % of 40 kWh (10 kWh) hedge wildly different *absolute*
   risk. The risk is an energy quantity.
2. **Not configurable.** `main.go` forced `socSafety = 25` on any value `<= 0`,
   so `0` (documented as "disable") became 25 %, and sub-`SoCMinPct` values were
   silently clamped — you couldn't run it low or off.
3. **Fights the forecast.** A floor is a separate penalty bolted onto the DP;
   it can suppress legitimate "run down now, refill at a cheap/negative-price
   hour later" decisions because it doesn't see the price/PV forecast.

The design discussion converged: the floor is the wrong mechanism. Fix the
economics instead of adding a magic number (cf. the "no optimizer hedge bonuses"
principle).

## Design (Alt 2): plan against downside PV

Remove the SoC/energy floor entirely. Instead, the DP plans against a
**pessimistic PV estimate**:

```
PV_plan(t) = max(0, PV_forecast(t) − k · σ)          (generation, W)
```

- **σ** = the live PV forecast-error std, `pvmodel.Service.ResidualStdW()`
  (sqrt-variance of recent predicted-vs-actual PV residuals). It grows on
  variable/cloudy days and shrinks to ~0 on clear, stable days.
- **k** = the one tunable, `pv_forecast_safety_k` (default **1.0** ≈ planning to
  the P16 downside). `k = 0` → raw forecast, no hedge ("use the battery you
  have").
- Applied in `service.go:applyPVDownside(slots, k, σ)` after `buildSlots`, then
  the existing DP (`Optimize`) runs unchanged against the conservative PV.

### Why this satisfies every concern at once

- **Reserve emerges from the economics, sized to the real risk.** Planning
  against less PV, the DP won't empty the battery betting on a refill — a
  reserve appears, as large as the actual forecast uncertainty, not a flat %.
- **Battery-size-agnostic.** The haircut is in W of generation; the resulting
  reserve has nothing to do with cell size.
- **Self-tuning to weather.** Clear stable day → σ≈0 → use the battery freely.
  Variable day → σ large → keep more back.
- **Winter / no sun: naturally inert.** PV≈0 and σ≈0 → no haircut → passive runs
  its charge-cheap / discharge-for-self-consumption loop down to `SoCMinPct`
  unhindered. No special-casing.
- **Doesn't fight negative prices.** The DP still sees the full price forecast
  and prefers charging at cheap/negative hours; there's no separate penalty
  term to override that.

### Cost of the approach (accepted)

Planning pessimistic PV can cause some **unnecessary cheap-hour import** (the DP
grid-charges fearing PV won't come, then PV arrives and the surplus is exported
reactively). It is bounded by `k·σ` (a modest haircut, never zeroing real PV)
and the reactive EMS captures the live upside. `k` tunes the trade-off.

## Changes

- **`mpc.Params`:** remove `SoCSafetyFloorPct`, `SafetyFloorPenaltyOreKwhHour`.
- **`mpc/mpc.go`:** remove the safety-floor penalty block from `Optimize`.
- **`mpc/service.go`:** add `applyPVDownside(slots, k, σ)`; add Service fields
  `PVUncertaintyW func() float64` and `PVForecastSafetyK float64`; apply the
  haircut after `buildSlots`.
- **`pvmodel/service.go`:** add `ResidualStdW()` (σ accessor over the existing
  residual buffer).
- **`mpc/diagnose.go`:** drop the floor fields from the diagnostic snapshot +
  restore-merge.
- **`config.go`:** deprecate `soc_safety_floor_pct` /
  `safety_floor_penalty_ore_kwh_hour` (still parsed, ignored with a warning);
  add `pv_forecast_safety_k *float64` (pointer: unset → 1.0, explicit 0 → off).
- **`main.go`:** wire `mpcSvc.PVUncertaintyW = pvSvc.ResidualStdW` and
  `PVForecastSafetyK` (nil → 1.0); drop the floor resolution + Params
  assignment; warn when the deprecated keys are present.

## Testing (TDD)

- `applyPVDownside`: haircuts generation by `k·σ`, floors at 0, leaves night
  slots (PVW=0) untouched; no-op when `k=0` or `σ=0`.
- Integration: in a scenario where the only profitable refill after a morning
  export is free midday PV, the full-PV plan empties the battery (banking on the
  refill) while the downside-PV plan keeps a higher reserve.
- Replaced the two obsolete `TestSelfConsumptionSafetyFloor*` tests; repointed
  the snapshot-merge tests to `PVChargeBonusOreKwh`.
- `config`/`main`: pointer resolution (unset → 1.0, explicit 0 honored);
  deprecated-key warning.

## Future refinements (out of scope)

- **Lead-time-scaled σ.** `ResidualStdW` is a recent-error scalar applied flat
  across the horizon; a far-ahead slot is no more discounted than the next hour.
  Scaling σ by forecast lead time would be more precise.
- **Asymmetric conservatism.** Be pessimistic about PV for the run-down decision
  but not for the grid-charge decision, to shave the cheap-hour over-import.
