// Household planner prefs for the Plan card: forecast trust slider,
// battery-export permission, and the four export sentences.
// Pure helpers — plan.js owns DOM and POST /api/planner/prefs.

export const TRUST_STEPS = ["cautious", "balanced", "bold"];

const SALE_W = 100;

export function sliderFromTrust(trust) {
  const i = TRUST_STEPS.indexOf(trust);
  return i >= 0 ? i : 1;
}

export function trustFromSlider(value) {
  const n = Number(value);
  return TRUST_STEPS[n] || "balanced";
}

export function safetyK(trust) {
  if (trust === "cautious") return 2;
  if (trust === "bold") return 0;
  return 1;
}

export function hedgeLine(k, sigmaW) {
  if (sigmaW == null || typeof sigmaW !== "number" || isNaN(sigmaW) || sigmaW < 0) return null;
  const sigma = Math.round(sigmaW);
  if (sigma < 1) return "σ right now ≈ 0 W — no hedge";
  let kn = parseFloat(k);
  if (isNaN(kn) || kn < 0) kn = 0;
  return "σ right now ≈ " + sigma + " W → hedge = k·σ ≈ " + Math.round(kn * sigma) + " W";
}

export function isBatterySale(action) {
  return (Number(action && action.battery_w) || 0) < -SALE_W
    && (Number(action && action.grid_w) || 0) < -SALE_W;
}

export function isGridExport(action) {
  return (Number(action && action.grid_w) || 0) < -SALE_W;
}

function clock(ms) {
  const d = new Date(ms);
  return String(d.getHours()).padStart(2, "0") + ":" +
    String(d.getMinutes()).padStart(2, "0");
}

export function batterySaleWindow(actions, nowMs) {
  const list = Array.isArray(actions) ? actions : [];
  const sale = list.filter(isBatterySale);
  if (!sale.length) return null;
  const now = nowMs == null ? Date.now() : nowMs;
  const upcoming = sale.filter((a) => (
    a.slot_start_ms + a.slot_len_min * 60_000 > now
  ));
  const block = upcoming.length ? upcoming : sale;
  let last = block[0];
  for (let i = 1; i < block.length; i++) {
    const expected = last.slot_start_ms + last.slot_len_min * 60_000;
    if (Math.abs(block[i].slot_start_ms - expected) > 1000) break;
    last = block[i];
  }
  const end = last.slot_start_ms + last.slot_len_min * 60_000;
  return { start: clock(block[0].slot_start_ms), end: clock(end) };
}

export function exportSentence({
  actions = [],
  exportPermission = "unknown",
  nowMs = Date.now(),
} = {}) {
  const window = batterySaleWindow(actions, nowMs);
  if (window) {
    return "Battery sale planned " + window.start + "–" + window.end + ".";
  }
  if (actions.some(isGridExport)) {
    return "Solar export only; the battery is not selling.";
  }
  if (exportPermission === "allowed") {
    return "Battery export is allowed, but FTW found no worthwhile sale.";
  }
  return "Battery sale blocked: permission is off or not checked.";
}

export function prefsFromStatus(status) {
  const s = status || {};
  const trust = TRUST_STEPS.includes(s.forecast_trust) ? s.forecast_trust : "balanced";
  const exp = s.battery_export;
  return {
    forecast_trust: trust,
    battery_export: (exp === "allowed" || exp === "not_allowed" || exp === "unknown")
      ? exp
      : "unknown",
    yaml_custom: !!s.planner_yaml_custom,
    mapped_k: typeof s.planner_mapped_k === "number" && !isNaN(s.planner_mapped_k)
      ? s.planner_mapped_k
      : safetyK(trust),
  };
}
