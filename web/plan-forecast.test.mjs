import assert from "node:assert/strict";
import { test } from "node:test";
import { forecastMarginLine, evShortfallWh } from "./plan-forecast.js";

test("forecast remains visible when the planning lower bound is zero", () => {
 const action = { slot_start_ms: 1000, slot_len_min:60, forecast_pv_w:-2314.6, forecast_load_w:1464.6, pv_w:0, load_w:3659.4 };
 assert.equal(forecastMarginLine([action],1000,3601000), "Shown intervals: PV forecast 2.3 kWh; plan uses 0.0 kWh. Load forecast 1.5 kWh; plan uses 3.7 kWh.");
 assert.equal(forecastMarginLine([{...action,forecast_pv_w:undefined}],1000,3601000),null);
 assert.match(forecastMarginLine([{...action,execution_start_ms:1801000}],1000,3601000),/PV forecast 1.2 kWh/);
});

test("EV shortfall preserves an unmet goal instead of calling it complete", () => {
 assert.equal(evShortfallWh({loadpoint_shortfall_wh:{garage:2318,other:NaN}}),2318);
 assert.equal(evShortfallWh({}),0);
});
