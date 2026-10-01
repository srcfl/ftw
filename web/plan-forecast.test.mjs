import assert from "node:assert/strict";
import { test } from "node:test";
import { forecastMargins, evShortfallWh } from "./plan-forecast.js";

const round = (m) => Object.fromEntries(Object.entries(m).map(([k, v]) => [k, Math.round(v * 1e4) / 1e4]));

test("margins keep the forecast visible when the planning lower bound is zero", () => {
  const action = { slot_start_ms: 1000, slot_len_min: 60, forecast_pv_w: -2314.6, forecast_load_w: 1464.6, pv_w: 0, load_w: 3659.4 };
  // Sun beyond use is 850 W for one hour; the plan counts on none of it.
  assert.deepEqual(round(forecastMargins([action], 1000, 3601000)), {
    sunBeyondKWh: 0.85, plannedSunBeyondKWh: 0, sunHeldKWh: 2.3146, useAddedKWh: 2.1948,
  });
  assert.equal(forecastMargins([{ ...action, forecast_pv_w: undefined }], 1000, 3601000), null);
  // Only the part of the slot still to run counts.
  assert.equal(round(forecastMargins([{ ...action, execution_start_ms: 1801000 }], 1000, 3601000)).sunHeldKWh, 1.1573);
});

test("sun beyond use is summed slot by slot, not from daily totals", () => {
  const hour = (start, pv, load) => ({ slot_start_ms: start, slot_len_min: 60,
    forecast_pv_w: pv, forecast_load_w: load, pv_w: pv, load_w: load });
  // Noon has 3 kWh spare, the evening needs 2 kWh: 3 kWh can reach the battery.
  const m = forecastMargins([hour(0, -4000, 1000), hour(3_600_000, 0, 2000)], 0, 7_200_000);
  assert.equal(m.sunBeyondKWh, 3);
  assert.equal(m.plannedSunBeyondKWh, 3);
  assert.equal(m.sunHeldKWh, 0);
});

test("EV shortfall preserves an unmet goal instead of calling it complete", () => {
  assert.equal(evShortfallWh({ loadpoint_shortfall_wh: { garage: 2318, other: NaN } }), 2318);
  assert.equal(evShortfallWh({}), 0);
});
