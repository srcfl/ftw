// What to call the number on a price. Every price the API returns is in
// minor units per kWh — öre for SEK, cent for EUR, øre for NOK — because
// go/internal/prices stores spot × 100 whatever the currency. Only the
// label and the sensible number of decimals differ, so they live here
// rather than being spelled "öre" in ten places.
//
// Pure data + pure functions: components import it, and it also lands on
// window.FTWUnits for the classic scripts (app.js, diagnose.js,
// loadpoints.js, the settings tabs) that can't import.

// scale is what a stored minor unit is multiplied by for display. 1 keeps
// minor units (öre, cent); 0.01 shows the major unit instead, which is how
// koruna, forint and leu are actually quoted — 4 Kč/kWh, not 400 haléř.
//
// axis is the cramped form for chart tick labels, where a three-digit price
// and its unit share about six characters.
const UNITS = {
  SEK: { label: "öre", perKwh: "öre/kWh", axis: "ö", scale: 1, decimals: 1 },
  NOK: { label: "øre", perKwh: "øre/kWh", axis: "ø", scale: 1, decimals: 1 },
  DKK: { label: "øre", perKwh: "øre/kWh", axis: "ø", scale: 1, decimals: 1 },
  EUR: { label: "cent", perKwh: "cent/kWh", axis: "c", scale: 1, decimals: 1 },
  PLN: { label: "gr", perKwh: "gr/kWh", axis: "gr", scale: 1, decimals: 1 },
  CHF: { label: "Rp.", perKwh: "Rp./kWh", axis: "Rp", scale: 1, decimals: 1 },
  CZK: { label: "Kč", perKwh: "Kč/kWh", axis: "Kč", scale: 0.01, decimals: 2 },
  HUF: { label: "Ft", perKwh: "Ft/kWh", axis: "Ft", scale: 0.01, decimals: 1 },
  RON: { label: "lei", perKwh: "lei/kWh", axis: "lei", scale: 0.01, decimals: 2 },
};

// A currency with no entry above is shown in its major unit under its ISO
// code — never wrong, just less familiar than "öre".
function fallback(code) {
  const c = String(code || "").toUpperCase();
  return { label: c, perKwh: c + "/kWh", axis: c, scale: 0.01, decimals: 3 };
}

// unitFor returns { label, perKwh, scale, decimals } for a currency code.
// An empty or unknown code falls back to SEK, which is what installs
// predating the currency setting are in.
export function unitFor(currency) {
  const code = String(currency || "SEK").toUpperCase();
  return UNITS[code] || fallback(code);
}

// The unit on its own: "öre", "cent", "Kč".
export function unitLabel(currency) {
  return unitFor(currency).label;
}

// The unit per kWh: "öre/kWh", "cent/kWh".
export function unitPerKwh(currency) {
  return unitFor(currency).perKwh;
}

// A stored minor-unit value in display units, unrounded.
export function toDisplay(minorPerKwh, currency) {
  return (minorPerKwh || 0) * unitFor(currency).scale;
}

// A stored minor-unit value as text with its unit: "17.4 öre".
// decimals overrides the currency's own default when a surface wants
// whole numbers.
export function formatPrice(minorPerKwh, currency, decimals) {
  const u = unitFor(currency);
  const d = decimals == null ? u.decimals : decimals;
  return toDisplay(minorPerKwh, currency).toFixed(d) + " " + u.label;
}

// Same, but with the /kWh suffix: "17.4 öre/kWh".
export function formatPricePerKwh(minorPerKwh, currency, decimals) {
  const u = unitFor(currency);
  const d = decimals == null ? u.decimals : decimals;
  return toDisplay(minorPerKwh, currency).toFixed(d) + " " + u.perKwh;
}

// ---- The install's own currency ----
//
// Surfaces that show a price but never fetch one — the plan tooltip in
// app.js, the diagnose timeline, the loadpoint schedule — read it from
// here instead of each asking the API. Whoever reads /api/prices sets it;
// bootstrapCurrency covers a page that opens straight onto one of those
// views. Until it resolves the answer is SEK, which is what every install
// predating the currency setting is in.
let active = "SEK";

export function activeCurrency() {
  return active;
}

export function setActiveCurrency(code) {
  if (code) active = String(code).toUpperCase();
  return active;
}

// Asks the price API for the currency alone — an empty time window, so the
// answer carries no price rows. Failure leaves the SEK default in place.
export function bootstrapCurrency(fetchImpl) {
  const f = fetchImpl || (typeof fetch === "function" ? fetch : null);
  if (!f) return Promise.resolve(active);
  return f("/api/prices?since_ms=0&until_ms=0")
    .then((r) => r.json())
    .then((j) => setActiveCurrency(j && j.currency))
    .catch(() => active);
}

if (typeof window !== "undefined") {
  window.FTWUnits = {
    unitFor,
    unitLabel,
    unitPerKwh,
    toDisplay,
    formatPrice,
    formatPricePerKwh,
    activeCurrency,
    setActiveCurrency,
  };
}
