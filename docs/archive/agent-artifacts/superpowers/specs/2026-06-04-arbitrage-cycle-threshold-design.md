# Arbitrage cycle threshold (minimum arbitrage spread) — design

**Date:** 2026-06-04
**Status:** approved — implementing (TDD)
**Area:** `go/internal/mpc`, `go/internal/config`, `web/settings/tabs/planner.js`

## Problem

Field testers (Anders, Björn, xorath, via Discord) run active arbitrage and
want to stop the battery cycling for a gain of just a few öre. The current
workaround — inflating the grid tariff to dampen cycling — corrupts the
savings statistics (xorath's point): the savings module computes real spot
economics, so anything that distorts the price distorts the reported
savings. We need a knob that biases the *planner's decision* without
touching the savings accounting.

## Goals

- A single operator knob: **minimum arbitrage spread (öre/kWh)**, "don't
  cycle the battery for a gain smaller than this".
- Affects the **planner decision only** — the reported savings / cost
  (`Plan.CostOre`, the savings module) stay on real spot economics.
- Applies to **both arbitrage modes** (`planner_arbitrage` +
  `planner_passive_arbitrage`). Self-consumption / cheap-charge untouched
  (their spread is retail + VAT, dwarfing any öre-level threshold).
- Default 0 = disabled → zero behaviour change for existing installs.

## Non-goals

- Changing the savings module, the price forecast, or the export
  bonus/fee economics. The threshold is purely a planner bias.
- A per-slot or time-of-day variable threshold. One scalar for v1.
- Suppressing self-consumption or cover-load discharge (retail spread
  always beats the threshold; no special-casing needed).

## Design

### 1. The knob → a DP throughput cost

Operator sets `S` öre/kWh. In the MPC DP action loop (`mpc.go`), for a
**discharge** action (`battW < 0`) in an **arbitrage mode**, add a virtual
cost proportional to the AC-side discharge throughput:

```go
if (p.Mode == ModeArbitrage || p.Mode == ModePassiveArbitrage) &&
    p.MinArbitrageSpreadOreKwh > 0 && battW < 0 {
    dischargeKWh := -battW * dtH / 1000.0 // positive magnitude
    cost += p.MinArbitrageSpreadOreKwh * dischargeKWh
}
```

This sits alongside the existing mode-gated cost terms (`PVChargeBonus`,
safety-floor penalty, the self-consumption house-import bias) in the same
action loop. Effect: the DP only schedules an arbitrage discharge when the
slot's revenue beats the stored energy's opportunity cost **by at least
`S` per kWh**, on top of the round-trip efficiency loss the physics
already imposes. Taxing discharge alone suppresses the whole cycle —
there's no point charging for a discharge that won't clear the threshold.

**Why discharge-only (not charge+discharge):** a round-trip's profit is
realised on discharge; charging is only valuable if the energy can later
be discharged profitably. A single discharge-side term maps the operator's
"öre/kWh" directly and keeps the math/UX simple.

### 2. Savings isolation (xorath's requirement) — automatic

The virtual cost enters only the DP's internal objective (`V[t][s]`). The
reported per-slot cost is recomputed from the **raw** grid flow via
`SlotGridCostOre(slot, gridKWh, p)` (`mpc.go` plan-emit, ~line 892), which
never sees `MinArbitrageSpreadOreKwh`. So the threshold changes *which
slots cycle* but never the reported `Plan.CostOre` / savings. This mirrors
the existing rule "Plan.TotalCostOre is the raw-price re-scoring, not the
DP objective" (mpc/CLAUDE.md).

### 3. Config + wiring

- `config.Planner` gains `MinArbitrageSpreadOreKwh float64`
  (`yaml/json: min_arbitrage_spread_ore_kwh,omitempty`), default 0.
- `mpc.Params` gains `MinArbitrageSpreadOreKwh float64`.
- `mpc.Service` gains the field; `buildParams` copies it into `Params`
  (exactly like `ExportBonusOreKwh` at `service.go:~862`).
- `main.go` wires `cfg.Planner.MinArbitrageSpreadOreKwh` into the service
  alongside the other planner params.

### 4. UI

`web/settings/tabs/planner.js` — add one field in the planner fieldset
using the existing `field()` helper:

```js
field("Min arbitrage spread (öre/kWh)", "planner.min_arbitrage_spread_ore_kwh",
  "number", 0,
  "The battery won't cycle for grid arbitrage unless the price gain beats " +
  "this many öre/kWh, on top of round-trip losses. 0 = off. Higher = fewer, " +
  "deeper cycles. Self-consumption is never affected. Tune empirically.")
```

The config round-trips through `POST /api/config` as JSON, so the matching
`json` tag on `Planner` is all the plumbing the field needs.

### 5. Semantics (documented in help text)

`S` is an **additional deadband on top of round-trip break-even**, applied
per kWh discharged. The round-trip loss (~5–10 öre, price-dependent) is
already imposed by physics; `S` stacks on top. Operators tune it
empirically (Björn: "labba lite med värdena").

## Testing (TDD)

`go/internal/mpc/*_test.go`, driving `Optimize` directly with hand-built
slots:

1. **Baseline cycles on a small spread.** `S=0`, two-slot arbitrage with a
   modest spread that clears round-trip break-even → plan charges then
   discharges.
2. **Threshold suppresses the marginal cycle.** Same slots, `S` set above
   the spread → plan holds (no discharge-for-arbitrage).
3. **Large spread still cycles.** Same `S`, a wide spread → plan still
   cycles (threshold is a deadband, not a hard stop).
4. **Cover-load / self-consumption discharge unaffected.** A discharge that
   offsets house load at retail+VAT spread is not suppressed even with a
   non-trivial `S` (passive_arbitrage mode).
5. **Savings isolation.** `Plan.CostOre` (and `TotalCostOre`) for a given
   dispatch are identical with `S=0` and `S>0` — the reported cost is
   independent of the threshold.
6. **Mode gating.** `S>0` in `ModeSelfConsumption` / `ModeCheapCharge` is a
   no-op (the term only applies in the two arbitrage modes).

`go/internal/config/config_test.go`: round-trip + default-zero for the new
field (and any validation if a negative value should be rejected → clamp
or error; v1: reject negative in `Validate`).

## Rollout

- Additive, default 0 → no behaviour change until an operator sets it.
- Changeset: `minor` (new planner config + UI field + planner behaviour).
