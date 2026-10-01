// The point forecast and the inputs after the margin belong to the same plan.
// Legacy PV-model residuals cannot describe the active forecast's margin.
//
// forecastMargins sums the shown window: sun beyond the home's own use, as
// forecast and as the plan counts on it, and the margin split into sun held
// back and use added. null when the plan lacks either set of values.
export function forecastMargins(actions, from, until) {
  let sunBeyond = 0, plannedSunBeyond = 0, sunHeld = 0, useAdded = 0, count = 0;
  for (const a of actions || []) {
    const start = Math.max(a.slot_start_ms, a.execution_start_ms || 0, from);
    const end = Math.min(a.slot_start_ms + a.slot_len_min * 60_000, until);
    if (end <= start) continue;
    if (![a.forecast_pv_w, a.forecast_load_w, a.pv_w, a.load_w].every(Number.isFinite)) return null;
    const hours = (end - start) / 3_600_000;
    const sun = Math.max(0, -a.forecast_pv_w), plannedSun = Math.max(0, -a.pv_w);
    const use = Math.max(0, a.forecast_load_w), plannedUse = Math.max(0, a.load_w);
    sunBeyond += Math.max(0, sun - use) * hours / 1000;
    plannedSunBeyond += Math.max(0, plannedSun - plannedUse) * hours / 1000;
    sunHeld += Math.max(0, sun - plannedSun) * hours / 1000;
    useAdded += Math.max(0, plannedUse - use) * hours / 1000;
    count++;
  }
  return count ? { sunBeyondKWh: sunBeyond, plannedSunBeyondKWh: plannedSunBeyond, sunHeldKWh: sunHeld, useAddedKWh: useAdded } : null;
}

export function evShortfallWh(plan) {
  return Object.values(plan?.loadpoint_shortfall_wh || {}).reduce((total, wh) => total + (Number.isFinite(wh) ? Math.max(0, wh) : 0), 0);
}
