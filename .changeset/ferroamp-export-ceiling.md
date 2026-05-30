---
"forty-two-watts": minor
---

**Add `site.max_export_w` — an opt-in site export ceiling below the
physical fuse.** Some inverters trip into a protective fault on
*sustained* grid export well under the breaker rating: the Ferroamp
EnergyHub faults to state `0x8030` after ~8 kW of continuous midday
export (battery discharge stacked on PV surplus) and only recovers as PV
wanes — losing hours of solar. Recurred daily on a live
`planner_arbitrage` site whose plan discharged the battery into the
morning price peak while PV was already exporting; grid voltage and
frequency were both in spec at every trip, ruling out a normal grid
protection.

`max_export_w` (W, magnitude; `0` = disabled, the default) is enforced
on two layers:

- **Dispatch** — the fuse guard's export side now scales battery
  discharge against `min(fuse − margin, max_export_w)` via the new
  `(*State).effectiveExportCeilingW`, mirroring the import-side
  `effectiveImportCeilingW` / `peak_import_ceiling_w`. Hot-reloadable.
- **MPC** — every plan slot's export limit becomes
  `min(FuseMaxW, max_export_w)` (`clampSlotGridLimits`), so the DP never
  *schedules* a discharge that would over-export — fixing the root cause
  rather than only clamping at execution time. Applied at startup
  (parity with the existing per-slot fuse plumbing).

Off by default; existing sites are unaffected until they set the knob.
The full-battery, PV-only over-export case still needs PV curtailment —
the discharge clamp can only scale battery action, not PV.
