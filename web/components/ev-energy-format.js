// Display helpers for EV charging energy and cost. The API reports energy in
// Wh and cost in minor currency units (öre, cent, …), the same units as the
// rest of the price and savings surfaces.

export function formatEnergyKWh(wh) {
  const kwh = Math.max(0, Number(wh) || 0) / 1000;
  if (kwh >= 100) return kwh.toFixed(0) + " kWh";
  if (kwh >= 10) return kwh.toFixed(1) + " kWh";
  return kwh.toFixed(2) + " kWh";
}

// Minor units in, major units out, with the site currency code.
export function formatCostMajor(minor, currency) {
  const major = (Number(minor) || 0) / 100;
  const absolute = Math.abs(major);
  const digits = absolute >= 100 ? 0 : absolute >= 10 ? 1 : 2;
  const sign = major < 0 ? "−" : "";
  return sign + absolute.toFixed(digits) + " " + (currency || "SEK");
}

// A missing price must not look like a free charge. A partial price keeps
// the priced amount and says so.
export function formatEVCost(costMinor, unpricedWh, currency) {
  const unpriced = Number(unpricedWh) || 0;
  const cost = Number(costMinor) || 0;
  if (unpriced > 1e-3 && Math.abs(cost) < 1e-6) return "No price";
  const text = formatCostMajor(cost, currency);
  if (unpriced > 1e-3) return text + " (partial)";
  return text;
}

export function windowById(windows, id) {
  return (Array.isArray(windows) ? windows : []).find((row) => row && row.id === id) || null;
}

export function attributionNote(attribution) {
  if (attribution === "import_price") {
    return "No site meter was recorded for these hours, so charging is priced as grid import.";
  }
  if (attribution === "mixed") {
    return "Cost is the grid share of charging where the site meter was recording. Hours without it are priced as grid import.";
  }
  return "Cost is the grid share of this charging, at the price the site paid. Solar and home-battery energy is not billed again.";
}

export function costFootnote(window) {
  const note = attributionNote(window && window.attribution);
  if (window && window.cost_partial) {
    return note + " Some hours have no electricity price, so the cost leaves them out.";
  }
  return note;
}

export function chargerName(charger) {
  if (!charger) return "";
  return charger.loadpoint_id || charger.label || charger.asset_id || "Charger";
}
