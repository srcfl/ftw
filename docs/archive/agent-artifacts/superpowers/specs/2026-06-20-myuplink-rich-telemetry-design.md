# MyUplink rich telemetry + grouped drill-in — design

**Date:** 2026-06-20
**Status:** approved (Fredrik, 2026-06-20)
**Workstream:** heating / thermal systems — observability layer (precedes Step 2)
**Builds on:** `2026-06-15-myuplink-telemetry-design.md` (read-only Step 1, shipped)

## Motivation

The MyUplink driver currently reads four fixed parameters (compressor power +
three temperatures). The MyUplink API exposes **dozens to ~100 points** per
heat pump (supply/return, brine, degree-minutes, compressor frequency,
electrical, hot-water charge, setpoints, operating mode, …). We want all of it
flowing and observable — both as a first-class operator view and as the data
that will ground the later thermal-store model.

A heat pump is **not** a battery/PV/meter. It must not be shoehorned into the
DER model. Its rich signals live as long-format TS metrics; the DER reading
slots (battery SoC / PV) must not render for it.

## Decisions (brainstorm, 2026-06-20)

1. **Purpose:** observability now **and** groundwork for the thermal model.
2. **Capture:** all points, auto-grouped (not a curated subset).
3. **UI:** at-a-glance summary card + click-through detail view, grouped by unit.
4. **Thermal model is a separate follow-up spec** — it needs days of this real
   telemetry before it can be fitted. This spec ships the data + the view.

## Scope of THIS spec

Rich telemetry capture + a grouped drill-in view + small cleanups. The
thermal-store model + compressor control (Step 2–4) are explicitly **out**.

## Design

### 1. Host — `emit_metric` gains an optional unit

`host.emit_metric(name, value [, unit])`. The unit threads through
`telemetry.Store.EmitMetric(driver, name, value, unit)` into the existing
`ts_metrics.unit` column and into the live `MetricSnapshot` (add a `Unit`
field). Backward compatible: existing 2-arg calls pass `unit=""`.

- `MetricSnapshot` gains `Unit string `json:"unit,omitempty"``.
- `Store.EmitMetric` signature gains `unit string`; all existing Go callers
  pass `""` (none today besides the Lua bridge).
- The Lua bridge (`host.emit_metric`) reads an optional 3rd string arg.

### 2. Driver — fetch all points

`driver_poll` switches from `GET /points?parameters=<ids>` to
`GET /v2/devices/{id}/points` (no filter) → the full array. For each point
whose `value` parses as a number:

- Metric name: `hp_` + **sanitize(parameterName)** — lowercase, every run of
  non-`[a-z0-9]` → `_`, trimmed, collapsed. On a sanitize collision, append
  `_` + `parameterId`.
- Unit: `parameterUnit`.
- Emit `host.emit_metric(name, value, unit)`.

The four canonical headline metrics (`hp_power_w`, `hp_hw_top_temp_c`,
`hp_indoor_temp_c`, `hp_outdoor_temp_c`) are still emitted from their fixed
parameter IDs (so the dashboard card + its history stay stable). Those four
parameter IDs are **skipped** in the generic all-points loop to avoid duplicate
metrics.

Enum/state points: emit the numeric `value` (the enum index) with its unit;
human enum-label decoding is a later nice-to-have (noted, not built).

Cardinality note: ~50–100 metrics at 1/min. The TS DB + hourly parquet rolloff
already handle this; no schema change beyond the unit column (which exists).

### 3. API — surface the unit

`GET /api/drivers/{name}` already returns `metrics`; each entry now includes
`unit` (from the `MetricSnapshot.Unit` added in §1). `GET /api/series` and
`GET /api/series/catalog` are reused as-is for the sparklines. No new endpoint.

### 4. UI — summary card + grouped drill-in

- The dashboard **Heat pump** card (existing `web/heating.js`) is unchanged in
  content (4 headline values + 24 h power sparkline) but becomes **clickable**
  → opens a heat-pump **detail modal**.
- The detail modal fetches `GET /api/drivers/{name}` (metrics + units) and
  groups rows by **unit class**:
  - Temperatures (`°C`)
  - Power (`W`, `kW`)
  - Frequency (`Hz`)
  - Percent (`%`)
  - Counters / degree-minutes (`h`, `GM`/`DM`, counts)
  - State / other (no unit / enum)
- Each row: de-sanitized label, current value + unit, a 24 h mini-sparkline
  from `GET /api/series?driver=<name>&metric=<name>&range=24h`.
- Styling reuses the diagnostics-modal tokens/patterns (mono numerics, hairline
  rows, theme `var(--*)` colours, amber sparkline). Follows `DESIGN.md`.
- Implementation lives in `web/heating.js` (+ a small style block), self-
  contained like the existing card.

### 5. Cleanup — no DER zero-slots for non-DER drivers

Audit the web UI for any place that renders a battery-SoC / PV slot per driver
and shows `0` for a driver that emits no such DER reading (the MyUplink driver
emits none — `readings` is null, capabilities are `apicreds` only). Render
those slots only when the driver actually has the corresponding DER reading.
(Exact location to be pinned during implementation — confirm with Fredrik
where he observed it.)

## Out of scope (next spec)

Thermal-store model (stored-energy proxy from tank temp + degree-minutes), COP
estimation (electrical-in vs heat-out), and the compressor control primitive
(block / shed / shift) for MPC as a deferrable thermal load. These get their
own spec, fed by the telemetry this spec produces.

## Testing

- Host: `emit_metric(name, value, unit)` stores the unit; `MetricSnapshot`
  carries it; 2-arg calls still work.
- Driver: a fake `/points` payload with mixed units → all numeric points land
  as `hp_*` metrics with units; the four canonical names still emit; the four
  canonical parameter IDs are not double-emitted.
- UI: covered by manual verification against a running instance with live
  heat-pump metrics (grouping, sparklines, click-through). A node-level unit
  test of the pure grouping/sanitize helpers if they are factored out.

## Changeset

Minor (new capability surface: per-metric units + rich heat-pump view).
