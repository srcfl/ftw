// "Use the plan": the one control that hands the house to the planner, and
// turns the planner on first when it is off. Pure helpers with the fetch
// passed in, so tests drive them without a browser; plan.js owns the DOM.

// Permission to sell from the battery is a household answer, so a prefs read
// that fails or answers with nothing usable lands on the mode that never
// sells from the battery.
export const PLANNER_FALLBACK_MODE = "planner_passive_arbitrage";

// /api/mpc/plan reasons a tap cannot fix: the owner adds prices or a battery.
const BLOCKED = new Set(["no-price-provider", "no-battery-capacity"]);

const RESTART_WAIT_MS = 120000;

function isPlannerMode(mode) {
  return String(mode || "").startsWith("planner_");
}

// What the Plan card shows for the box's state.
//   mode     the control mode the box runs ("" before the first read)
//   running  /api/mpc/plan says the planner runs
//   reason   why it does not run; "" with running=false means the planner is
//            on in Settings and starts at the next restart
//   hasPlan     a plan with steps exists
//   hasBattery  the home has a battery the plan can use
// Choices about a plan wait for the first plan; selling from the battery is
// no question for a home without one. Manual modes stay in reach when one
// other than the default drives, so its button is on screen.
export function planCardView({ mode = "", running = false, reason = "", hasPlan = false, hasBattery = true } = {}) {
  const plannerMode = isPlannerMode(mode);
  const canStart = running || !BLOCKED.has(reason);
  return {
    useVisible: Boolean(mode) && (!plannerMode || (!running && canStart)),
    useEnabled: canStart,
    styleVisible: hasPlan,
    exportVisible: hasPlan && hasBattery,
    manualVisible: hasPlan || (Boolean(mode) && !plannerMode && mode !== "self_consumption"),
  };
}

async function answer(response) {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
  return body;
}

function postJSON(api, path, body, headers = {}) {
  return api(path, {
    method: "POST",
    headers: Object.assign({ "Content-Type": "application/json" }, headers),
    body: JSON.stringify(body),
  });
}

// Settings → Planner → Enabled → Save, done for the owner: the same
// whole-document write, guarded by the same ETag. Answers whether the
// planner was already on in Settings.
async function enablePlanner(api) {
  const read = await api("/api/config", { headers: { Accept: "application/json" } });
  const etag = read.headers && read.headers.get ? read.headers.get("ETag") : null;
  const cfg = await answer(read);
  if (cfg.planner && cfg.planner.enabled) return { wasOn: true };
  cfg.planner = Object.assign({}, cfg.planner, { enabled: true });
  await answer(await postJSON(api, "/api/config", cfg, etag ? { "If-Match": etag } : {}));
  return { wasOn: false };
}

// startPlan hands the house to the plan. When the planner does not run it
// turns it on and restarts FTW, because the box builds the planner at start.
// say(text) reports each step. Resolves to "mode" when only the mode
// changed and "restart" when FTW restarted; rejects with a readable message.
export async function startPlan({ api, say = () => {}, running = false, reason = "" }) {
  if (!running && BLOCKED.has(reason)) throw new Error("Planning cannot start yet.");
  let restart = false;
  let wasOff = false;
  if (!running) {
    say("Turning the planner on…");
    const { wasOn } = await enablePlanner(api);
    wasOff = !wasOn;
    // The box now says whether a start would plan, or what is missing.
    const plan = await answer(await api("/api/mpc/plan"));
    if (BLOCKED.has(plan.reason)) {
      const err = new Error("Planning cannot start yet.");
      err.reason = plan.reason;
      throw err;
    }
    restart = !plan.enabled;
  }
  let prefs = null;
  try {
    prefs = await answer(await api("/api/planner/prefs", { headers: { Accept: "application/json" } }));
  } catch (e) {
    prefs = null;
  }
  // A planner that was off never sold from the battery, so there is nothing
  // to "continue". Record the answer the box already acts on, or the card
  // would ask whether FTW may keep selling.
  if (wasOff && prefs && prefs.battery_export === "unknown") {
    prefs = await answer(await postJSON(api, "/api/planner/prefs", { battery_export: "not_allowed" }));
  }
  const mapped = prefs && prefs.mapped_mode;
  const mode = isPlannerMode(mapped) ? mapped : PLANNER_FALLBACK_MODE;
  await answer(await postJSON(api, "/api/mode", { mode }));
  if (!restart) return "mode";
  say("Restarting FTW to start the planner…");
  await answer(await api("/api/restart", { method: "POST" }));
  return "restart";
}

// waitForPlanner polls until the restarted box runs the planner, or says
// why it cannot. Errors while the box is down are expected and ignored.
export async function waitForPlanner({ api, sleep, now = Date.now, timeoutMs = RESTART_WAIT_MS }) {
  const started = now();
  await sleep(1500);
  while (now() - started < timeoutMs) {
    try {
      const response = await api(`/api/mpc/plan?_=${now()}`, { cache: "no-store" });
      if (response.ok) {
        const plan = await response.json();
        if (plan.enabled) return { running: true };
        if (plan.reason) return { running: false, reason: plan.reason };
      }
    } catch (e) {
      // The box is restarting.
    }
    await sleep(1000);
  }
  throw new Error("FTW has not come back yet. Reload this page in a minute.");
}
