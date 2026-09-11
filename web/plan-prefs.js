// Household planner prefs for the Plan card: the safety-k slider,
// battery-export permission, and the four export sentences.
// Pure helpers — plan.js owns DOM and POST /api/planner/prefs.
//
// The stored primitive is safety_k, the share of each slot's own forecast
// uncertainty the plan holds back. forecast_trust is the derived enum older
// clients read; it never drives anything here.

export const TRUST_STEPS = ["cautious", "balanced", "bold"];

export const SAFETY_K_MIN = 0;
export const SAFETY_K_MAX = 2;
export const SAFETY_K_STEP = 0.05;

const SALE_W = 100;

// safetyK is the legacy enum→k mapping, kept for servers that answer with
// forecast_trust and no safety_k.
export function safetyK(trust) {
  if (trust === "cautious") return 2;
  if (trust === "bold") return 0;
  return 1;
}

// trustFromSafetyK mirrors config.TrustFromSafetyK so the label under the
// slider matches the enum the box would report.
export function trustFromSafetyK(k) {
  const n = clampSafetyK(k);
  if (n <= 0.25) return "bold";
  if (n < 1.5) return "balanced";
  return "cautious";
}

export function clampSafetyK(v) {
  const n = typeof v === "number" ? v : parseFloat(v);
  if (isNaN(n)) return 1;
  if (n < SAFETY_K_MIN) return SAFETY_K_MIN;
  if (n > SAFETY_K_MAX) return SAFETY_K_MAX;
  return n;
}

// formatSafetyK renders the slider's position at its own resolution: 0.05
// steps need two decimals, and trailing zeros make the number look stuck.
export function formatSafetyK(k) {
  return String(Math.round(clampSafetyK(k) * 100) / 100);
}

// hedgeLine says what the current k costs in watts. sigmaRel (the PV twin's
// rel_mae) is the same number the per-slot haircut uses, so when it is known
// the line can name the share of every sunny slot held in reserve.
export function hedgeLine(k, sigmaW, sigmaRel) {
  if (sigmaW == null || typeof sigmaW !== "number" || isNaN(sigmaW) || sigmaW < 0) return null;
  const sigma = Math.round(sigmaW);
  if (sigma < 1) return "σ right now ≈ 0 W — no hedge";
  const kn = clampSafetyK(k);
  const line = "σ right now ≈ " + sigma + " W → hedge = k·σ ≈ " + Math.round(kn * sigma) + " W";
  const rel = typeof sigmaRel === "number" && !isNaN(sigmaRel) && sigmaRel > 0 ? sigmaRel : null;
  if (rel == null) return line;
  const share = Math.min(100, Math.round(kn * rel * 100));
  return line + " · holds back " + share + "% of each sunny slot";
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
  // safety_k is the primitive; planner_mapped_k carries the same number for
  // servers that predate the rename, and the enum mapping covers a box older
  // than both.
  let k = s.safety_k;
  if (typeof k !== "number" || isNaN(k)) k = s.planner_mapped_k;
  if (typeof k !== "number" || isNaN(k)) k = safetyK(trust);
  return {
    forecast_trust: trust,
    battery_export: (exp === "allowed" || exp === "not_allowed" || exp === "unknown")
      ? exp
      : "unknown",
    safety_k: clampSafetyK(k),
  };
}
