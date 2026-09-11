// Reconstruct planned battery SoC when a slot omits a finite soc.
// Plan JSON stores SoC as a 0–1 fraction. The on-box Plan chart used to
// treat a missing value as 0% (canvas NaN becomes the 0% axis) and skip
// the tooltip row, while the help report still printed Charge end from
// the in-memory Go plan.

export function finiteNumber(value) {
  return typeof value === "number" && Number.isFinite(value);
}

function storageEnergyWh(action) {
  const stored = action && action.storage_energy_wh;
  if (!stored || typeof stored !== "object" || Array.isArray(stored)) return null;
  let sum = 0;
  let any = false;
  for (const value of Object.values(stored)) {
    if (!finiteNumber(value)) continue;
    sum += value;
    any = true;
  }
  return any ? sum : null;
}

function clampFrac(soc) {
  if (!finiteNumber(soc)) return null;
  return Math.min(1, Math.max(0, soc));
}

function legacyPercentToFrac(pct) {
  if (!finiteNumber(pct)) return null;
  return pct / 100;
}

function integrateBatteryWh(batteryW, slotLenMin, chargeEff, dischargeEff) {
  if (!finiteNumber(batteryW) || !finiteNumber(slotLenMin) || slotLenMin <= 0) return 0;
  const hours = slotLenMin / 60;
  const charge = finiteNumber(chargeEff) && chargeEff > 0 ? chargeEff : 0.95;
  const discharge = finiteNumber(dischargeEff) && dischargeEff > 0 ? dischargeEff : 0.95;
  // Site sign: +W charges the battery. Efficiency matches mpc.Optimize.
  if (batteryW >= 0) return batteryW * hours * charge;
  return batteryW * hours / discharge;
}

export function actionSoC(action, {
  capacityWh = null,
  prevSoC = null,
  chargeEff = 0.95,
  dischargeEff = 0.95,
} = {}) {
  if (finiteNumber(action && action.soc)) return action.soc;
  const legacy = legacyPercentToFrac(action && action.soc_pct);
  if (legacy != null) return legacy;
  const storedWh = storageEnergyWh(action);
  if (finiteNumber(storedWh) && finiteNumber(capacityWh) && capacityWh > 0) {
    return clampFrac(storedWh / capacityWh);
  }
  if (!finiteNumber(prevSoC) || !finiteNumber(capacityWh) || capacityWh <= 0) return null;
  const deltaWh = integrateBatteryWh(
    action && action.battery_w,
    action && action.slot_len_min,
    chargeEff,
    dischargeEff,
  );
  return clampFrac(prevSoC + deltaWh / capacityWh);
}

export function fillPlanSoC(plan, opts = {}) {
  if (!plan || !Array.isArray(plan.actions)) return plan;
  const capacityWh = finiteNumber(plan.capacity_wh) ? plan.capacity_wh : null;
  let prevSoC = finiteNumber(plan.initial_soc)
    ? plan.initial_soc
    : legacyPercentToFrac(plan.initial_soc_pct);
  for (const action of plan.actions) {
    const soc = actionSoC(action, { capacityWh, prevSoC, ...opts });
    if (!finiteNumber(soc)) continue;
    action.soc = soc;
    prevSoC = soc;
  }
  return plan;
}
