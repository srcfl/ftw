// The point forecast and the inputs after the margin belong to the same plan.
// Legacy PV-model residuals cannot describe the active forecast's margin.
export function forecastMarginLine(actions, from, until) {
  let pv = 0, planningPV = 0, load = 0, planningLoad = 0, count = 0;
  for (const a of actions || []) {
    const start = Math.max(a.slot_start_ms, a.execution_start_ms || 0, from);
    const end = Math.min(a.slot_start_ms + a.slot_len_min * 60_000, until);
    if (end <= start) continue;
    if (![a.forecast_pv_w, a.forecast_load_w, a.pv_w, a.load_w].every(Number.isFinite)) return null;
    const hours = (end - start) / 3_600_000;
    pv += Math.max(0, -a.forecast_pv_w) * hours / 1000;
    planningPV += Math.max(0, -a.pv_w) * hours / 1000;
    load += Math.max(0, a.forecast_load_w) * hours / 1000;
    planningLoad += Math.max(0, a.load_w) * hours / 1000;
    count++;
  }
  return count ? `Shown intervals: PV forecast ${pv.toFixed(1)} kWh; plan uses ${planningPV.toFixed(1)} kWh. Load forecast ${load.toFixed(1)} kWh; plan uses ${planningLoad.toFixed(1)} kWh.` : null;
}

export function evShortfallWh(plan) {
  return Object.values(plan?.loadpoint_shortfall_wh || {}).reduce((total, wh) => total + (Number.isFinite(wh) ? Math.max(0, wh) : 0), 0);
}
