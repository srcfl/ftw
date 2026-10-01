// Household planner prefs for the Plan card: the planning style, the
// battery-export permission, and the sentences that explain both.
// Pure helpers — plan.js owns DOM and POST /api/planner/prefs.
//
// The stored primitive is safety_k, the share of each slot's own forecast
// uncertainty the plan holds back. The card offers five planning styles on
// that one number; Settings → Planner fine-tunes it. forecast_trust is the
// derived enum older clients read; it never drives anything here.

export const TRUST_STEPS = ["cautious", "balanced", "bold"];

export const SAFETY_K_MIN = 0;
export const SAFETY_K_MAX = 2;
export const SAFETY_K_STEP = 0.05;

// Left to right, as on the card: keeps the most in the battery, then counts
// more and more on the forecast. Balanced is the box default
// (config.SafetyKDefault) and leans toward the forecast; keep the two equal.
export const PLAN_STYLES = Object.freeze([
  {
    key: "very_careful",
    name: "Very careful",
    k: 1,
    text: "Plans for a poor day: much less sun and more use than forecast. Keeps the most in the battery.",
  },
  {
    key: "careful",
    name: "Careful",
    k: 0.6,
    text: "Plans for less sun and more use than forecast.",
  },
  {
    key: "balanced",
    name: "Balanced",
    k: 0.3,
    text: "Plans for a little less sun and a little more use than forecast. A good start for most homes.",
  },
  {
    key: "bold",
    name: "Bold",
    k: 0.15,
    text: "Plans close to the forecast.",
  },
  {
    key: "very_bold",
    name: "Very bold",
    k: 0,
    text: "Plans on the forecast as it is. Earns the most when it is right and costs more when it is wrong.",
  },
]);

export const SAFETY_K_DEFAULT = 0.3;

const SALE_W = 100;
// Below this a total reads as nothing on a 0.1 kWh display.
const KWH_EPS = 0.05;
// A slot counts as sunny when the forecast expects more than this.
const SUNNY_W = 200;
// Planned battery power below this is a discharge slot (mpc.IdleGateThresholdW).
const DISCHARGE_W = 100;

// safetyK is the legacy enum→k mapping, kept for servers that answer with
// forecast_trust and no safety_k. Mirrors config.ForecastTrust.SafetyK.
export function safetyK(trust) {
  if (trust === "cautious") return 2;
  if (trust === "bold") return 0;
  return SAFETY_K_DEFAULT;
}

// trustFromSafetyK mirrors config.TrustFromSafetyK so the enum the card
// reports matches the one the box would report.
export function trustFromSafetyK(k) {
  const n = clampSafetyK(k);
  if (n <= 0.25) return "bold";
  if (n < 1.5) return "balanced";
  return "cautious";
}

export function clampSafetyK(v) {
  const n = typeof v === "number" ? v : parseFloat(v);
  if (isNaN(n)) return SAFETY_K_DEFAULT;
  if (n < SAFETY_K_MIN) return SAFETY_K_MIN;
  if (n > SAFETY_K_MAX) return SAFETY_K_MAX;
  return n;
}

// formatSafetyK renders k at the fine-tune resolution: 0.05 steps need two
// decimals, and trailing zeros make the number look stuck.
export function formatSafetyK(k) {
  return String(Math.round(clampSafetyK(k) * 100) / 100);
}

// styleForK names the style a stored k belongs to. A k between two styles
// (set in Settings) shows the nearest one, and exact says it is not that
// style's own value.
export function styleForK(k) {
  const n = clampSafetyK(k);
  let style = PLAN_STYLES[0];
  for (const s of PLAN_STYLES) {
    if (Math.abs(s.k - n) < Math.abs(style.k - n)) style = s;
  }
  return { style, exact: Math.abs(style.k - n) < 0.001 };
}

function kwh(v) {
  return (Math.round(v * 10) / 10).toFixed(1) + " kWh";
}

// sunWindowLabel names the window the chart shows: the rest of today, today
// and tomorrow, or tomorrow.
export function sunWindowLabel(horizon, untilMs, tomorrowStartMs) {
  if (horizon === "tomorrow") return "Tomorrow";
  if (horizon === "today" || untilMs <= tomorrowStartMs) return "Rest of today";
  return "Today and tomorrow";
}

// sunLine answers "why is the plan not filling the battery from the sun?"
// with the two numbers that decide it.
export function sunLine(margins, label) {
  if (!margins) return null;
  if (margins.sunBeyondKWh < KWH_EPS) {
    return `${label}: the forecast expects no sun beyond what your home uses.`;
  }
  const head = `${label}: the forecast expects ${kwh(margins.sunBeyondKWh)} more sun than your home uses.`;
  const counted = Math.min(margins.plannedSunBeyondKWh, margins.sunBeyondKWh);
  if (counted >= margins.sunBeyondKWh - KWH_EPS) return `${head} The plan counts on all of it.`;
  return `${head} The plan counts on ${kwh(counted)} of it.`;
}

// marginSplitLine is the expert view in Settings → Planner: the same margin
// split into sun held back and use added.
export function marginSplitLine(margins) {
  if (!margins) return null;
  if (margins.sunHeldKWh < KWH_EPS && margins.useAddedKWh < KWH_EPS) {
    return "The current plan uses the forecast as it is.";
  }
  return `The current plan counts on ${kwh(margins.sunHeldKWh)} less sun and ${kwh(margins.useAddedKWh)} more use than forecast.`;
}

// extraSunLine says where sun beyond the plan goes, by dispatch's rule:
// live surplus may charge the battery up to the operator's cap
// (site.pv_surplus_absorb_soc_cap) when one is set, otherwise up to Core's
// per-slot live_pv_surplus_soc_cap, and never during a discharge slot. Above
// the slot's planned charge it is stored; otherwise it is exported. Silent
// when the box does not send either cap or the window has no sunny slot.
export function extraSunLine(actions, from, until, operatorCap) {
  if (!Number.isFinite(operatorCap)) return null;
  let sunny = 0;
  let stored = 0;
  for (const a of actions || []) {
    const start = Math.max(a.slot_start_ms, a.execution_start_ms || 0, from);
    const end = Math.min(a.slot_start_ms + a.slot_len_min * 60_000, until);
    if (end <= start) continue;
    if (!Number.isFinite(a.live_pv_surplus_soc_cap)) return null;
    const pv = Number.isFinite(a.forecast_pv_w) ? a.forecast_pv_w : a.pv_w;
    if (!(Math.max(0, -pv) > SUNNY_W)) continue;
    sunny++;
    const cap = operatorCap > 0 ? operatorCap : a.live_pv_surplus_soc_cap;
    const discharging = (Number(a.battery_w) || 0) < -DISCHARGE_W;
    if (!discharging && cap > (Number(a.soc) || 0) + 0.005) stored++;
  }
  if (!sunny) return null;
  if (stored === sunny) {
    return "If more sun comes than planned, FTW stores it in the battery instead of buying power later.";
  }
  if (stored === 0) return "If more sun comes than planned, it goes to the grid.";
  return "If more sun comes than planned, FTW stores some of it and the rest goes to the grid.";
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

// prefsKnown is true once the box has said which margin it runs. Until then
// the card checks no style rather than show a default as the box's choice.
export function prefsKnown(status) {
  const s = status || {};
  return Number.isFinite(s.safety_k) || Number.isFinite(s.planner_mapped_k) ||
    TRUST_STEPS.includes(s.forecast_trust);
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
