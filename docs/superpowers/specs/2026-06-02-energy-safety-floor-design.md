# Energy safety floor — design

**Date:** 2026-06-02
**Status:** approved direction (A1), pending spec review
**Area:** `go/internal/mpc`, `go/internal/config`, `go/cmd/forty-two-watts/main.go`

## Problem

The MPC safety floor is `soc_safety_floor_pct` (default **25 %**): a soft cost
penalty in the DP that fires when SoC would end a *PV-surplus* slot below the
floor percentage, nudging the planner to bank PV now instead of gambling the
night's reserve on uncertain midday PV (`mpc.go` safety-floor block; gated on
`pvSurplusW > 0`).

Two flaws:

1. **Wrong unit.** A percentage is relative to battery size. 25 % of a 5 kWh
   battery (1.25 kWh) and 25 % of a 40 kWh battery (10 kWh) hedge wildly
   different *absolute* risk — but the risk being hedged (a PV-miss forcing an
   expensive evening-peak import) is an **absolute energy quantity**, not a
   fraction of an arbitrary cell.
2. **Not actually configurable.** `main.go` forces `socSafety = 25` whenever
   the configured value is `<= 0`, so an operator cannot disable it (`0` →
   25 %) and a value below `SoCMinPct` is silently clamped up. `0` is
   overloaded as both "unset → default" and "the number zero". The same
   `<= 0 → default` override hits `safety_floor_penalty_ore_kwh_hour` (→ 100).

## Goal

Replace the SoC-percentage floor with an **energy floor (Wh)** anchored to
**forecast household load** — "keep enough stored energy to cover the load for
the next H hours, so a PV-miss today doesn't force expensive evening import."
Battery-size-agnostic, self-sizing to the household, and genuinely
configurable (including a clean disable).

### Non-goals (flagged, deliberately out of scope)

- **A2 (dynamic window).** Sizing the window to "until the next cheap/PV
  charging opportunity" is a future refinement, not this change.
- **Winter grid-charge hedge.** The penalty stays PV-surplus-gated, so it never
  motivates grid-charging. In winter / no-sun it is therefore inert — passive
  does not pre-charge a cheap-grid evening reserve via this mechanism. Whether
  the floor should motivate *cheap-grid* charging for `passive_arbitrage` is a
  separate, larger question.
- Re-architecting the hedge as a probabilistic forecast-uncertainty term in
  the DP economics (the "fix the economics, not a magic number" ideal). The
  agreed mechanism is a soft floor; we are fixing its *unit* and
  *configurability*, not its nature.

## Design

### Floor quantity

At DP slot `t`, the reserve to keep is the forecast **gross** household load
over a forward coverage window `H = safety_floor_hours`:

```
E_floor(t) = Σ  Slot.LoadW[k] · dt_h   for k in [t, t + H)        (Wh)
E_floor(t) = min(E_floor(t), usableWh)   where usableWh = (SoCMax − SoCMin)/100 · CapacityWh
```

- **Gross load, PV-blind on purpose.** The window deliberately does *not*
  subtract forecast PV. PV is exactly the production we are hedging against not
  arriving; counting on it inside the safety reserve would defeat the hedge.
- **`Slot.LoadW`** (site-sign, ≥ 0) is already on every slot the DP consumes,
  so `E_floor(t)` is a forward window-sum precomputed once before the Bellman
  pass (a suffix-style rolling sum over the horizon). No new forecast input.
- **Capped at usable capacity** so the floor can never demand more energy than
  the battery can physically hold; on a small battery facing a large evening
  load the floor saturates at "full", which is the correct safe choice there.

### DP penalty (structure unchanged)

Keep today's soft-penalty shape; only the threshold changes from a percentage
to `E_floor(t)`:

```
storedWh = battSoc2/100 · CapacityWh
if SafetyFloorHours > 0 and SafetyFloorPenaltyOreKwhHour > 0
   and pvSurplusW(slot) > 0
   and storedWh < E_floor(t):
       deficitKwh = (E_floor(t) − storedWh) / 1000
       cost += deficitKwh · dt_h · SafetyFloorPenaltyOreKwhHour
```

- **PV-surplus gating retained** (`pvSurplusW = −PVW − LoadW > 0`) — the floor
  never motivates grid import; without surplus the DP follows the normal
  never-import contract. (This is what makes it inert in winter — see
  non-goals.)
- Soft, not hard: it is a cost term the DP weighs against arbitrage value, so a
  large `E_floor` does not peg the battery full — it preferentially soaks up
  *free PV surplus* until the reserve is banked rather than exporting/deferring.

### `mpc.Params` changes

- Remove `SoCSafetyFloorPct float64`.
- Add `SafetyFloorHours float64` (coverage window, hours; `0` = disabled).
- Keep `SafetyFloorPenaltyOreKwhHour float64`.
- The DP needs the per-slot forward load-sum; computed inside `Optimize` from
  the slots it already has (no new `Params` input).

### Config schema + migration

`config.go` planner block:

- **New:** `safety_floor_hours *float64` — `yaml:"safety_floor_hours,omitempty"`.
  Pointer so we distinguish *unset* (→ default 10 h) from explicit `0`
  (→ disabled). Documented: "Keep enough stored energy to cover forecast load
  for this many hours, as a hedge against a PV-miss. `0` disables. Default 10."
- **Change:** `safety_floor_penalty_ore_kwh_hour` → `*float64` too, so explicit
  `0` disables (today `0` is overridden to 100). Unset → default 100.
- **Deprecate:** `soc_safety_floor_pct` stays in the struct (parsed) but is no
  longer the mechanism. **Migration (recommended):** if it is explicitly set
  (`> 0`) and `safety_floor_hours` is unset, log a one-line deprecation warning
  and honor the **legacy %-floor** for that instance for one release (dual
  path), so a tuned field instance behaves identically until its operator
  migrates. Unset / new instances get the energy floor at the 10 h default.
  *(Open decision — see below: dual-path vs. straight replace-with-warning.)*

`main.go` wiring (`~2417`): drop the `if socSafety <= 0 { socSafety = 25 }`
override; resolve the pointer fields with `nil → default`, value used verbatim
(including `0`).

### Diagnose / API

`mpc/diagnose.go` currently surfaces `SoCSafetyFloorPct` +
`SafetyFloorPenaltyOreKwhHour`. Replace the first with `SafetyFloorHours`, and
(optionally) expose the **resolved `E_floor` for the current slot** in Wh so
operators can see the live reserve the planner is holding. Keep the snapshot
back-compat shim pattern already in `diagnose.go` for older persisted params.

## Behaviour examples

- **Battery-size-agnostic (the headline).** Same house (≈ 0.5 kW avg load), two
  sites: a 5 kWh and a 40 kWh battery, `safety_floor_hours = 10`. Both reserve
  the *same* ≈ 5 kWh of forecast load — 100 % of the small battery (saturates
  at full) and 12.5 % of the large one. The old 25 % gave 1.25 kWh vs 10 kWh.
- **Disable.** `safety_floor_hours: 0` → no penalty term. (Previously
  impossible — `0` became 25 %.)
- **Low reserve.** `safety_floor_hours: 2` → ≈ 1 kWh reserve, honored, not
  clamped. (Previously sub-`SoCMinPct` values were silently raised.)
- **Winter / no sun.** No PV-surplus slots → penalty never fires → floor inert
  → passive runs its charge-cheap / discharge-for-self-consumption loop down to
  `SoCMinPct` unhindered. (Unchanged from today.)

## Testing (TDD)

`mpc` (`mpc_test.go`):
- `E_floor` window-sum is computed from `Slot.LoadW` over `H`, capped at usable
  capacity.
- Penalty fires when `storedWh < E_floor` on a PV-surplus slot; does **not**
  fire on a non-surplus slot (winter), when `SafetyFloorHours == 0`, or when
  the penalty is `0`.
- **Battery-size invariance:** identical load forecast + `H`, two `CapacityWh`
  values → identical `E_floor` Wh (different implied %).

`config` (`config_test.go`):
- `safety_floor_hours` / penalty parse as pointers; unset → defaults; explicit
  `0` → disabled (distinguished from unset).
- Migration: `soc_safety_floor_pct` set + `safety_floor_hours` unset →
  deprecation path chosen below.

`cmd/forty-two-watts`:
- Pointer resolution: unset → 10 h / 100 öre; `0` honored as disabled; no
  `<= 0 → default` override remains.

## Open decision for review

**Migration path for an explicitly-set `soc_safety_floor_pct`:**

- **(i) Dual-path for one release (recommended).** Honor the legacy %-floor when
  it is explicitly set and `safety_floor_hours` is unset; warn deprecated.
  Existing tuned instances unchanged; remove the legacy path in a later major.
  Safer, slightly more code.
- **(ii) Replace + warn.** Remove the %-floor entirely; if `soc_safety_floor_pct`
  is present, warn it is ignored and the energy floor (default 10 h) applies.
  Simpler; changes behaviour for the (rare — not in `config.example.yaml`)
  instances that set it explicitly.

Recommendation: **(i)**, matching the "safe than sorry" stance — no field
instance silently changes behaviour on upgrade.
