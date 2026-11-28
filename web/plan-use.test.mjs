import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { planCardView, startPlan, waitForPlanner, PLANNER_FALLBACK_MODE } from "./plan-use.js";

// A box behind fetch: routes answer from a table, every call is recorded.
function fakeBox(routes) {
  const calls = [];
  const api = async (path, opts = {}) => {
    const method = opts.method || "GET";
    const body = opts.body ? JSON.parse(opts.body) : undefined;
    calls.push({ method, path, body, headers: opts.headers || {} });
    const route = routes[`${method} ${path.split("?")[0]}`];
    if (!route) throw new Error(`no route ${method} ${path}`);
    const answer = typeof route === "function" ? route(body) : route;
    const status = answer.status || 200;
    return {
      ok: status < 300,
      status,
      headers: { get: (name) => (answer.headers || {})[name] || null },
      json: async () => answer.json || {},
    };
  };
  return { api, calls };
}

const offBox = (extra = {}) => fakeBox({
  "GET /api/config": { headers: { ETag: '"r7"' }, json: { site: { name: "Home" }, price: { zone: "SE3" } } },
  "POST /api/config": { json: { status: "ok", restart_required: true } },
  "GET /api/mpc/plan": { json: { enabled: false } },
  "GET /api/planner/prefs": { json: { battery_export: "unknown", mapped_mode: "planner_passive_arbitrage" } },
  "POST /api/planner/prefs": (body) => ({ json: { battery_export: body.battery_export, mapped_mode: "planner_passive_arbitrage" } }),
  "POST /api/mode": { json: { status: "ok" } },
  "POST /api/restart": { status: 202, json: { status: "restarting" } },
  ...extra,
});

describe("Plan card view", () => {
  it("offers Use the plan on a new box with the planner off, and nothing else", () => {
    const view = planCardView({ mode: "self_consumption", running: false, reason: "planner-disabled" });
    assert.deepEqual(view, { useVisible: true, useEnabled: true, styleVisible: false, exportVisible: false, manualVisible: false });
  });

  it("offers it in a planner mode while the planner is off", () => {
    const view = planCardView({ mode: "planner_passive_arbitrage", running: false, reason: "planner-disabled" });
    assert.equal(view.useVisible, true);
    assert.equal(view.useEnabled, true);
  });

  it("hides it while the planner drives", () => {
    assert.equal(planCardView({ mode: "planner_passive_arbitrage", running: true, hasPlan: true }).useVisible, false);
  });

  it("keeps it off where a tap cannot help", () => {
    for (const reason of ["no-price-provider", "no-battery-capacity"]) {
      assert.equal(planCardView({ mode: "self_consumption", reason }).useEnabled, false);
      assert.equal(planCardView({ mode: "planner_passive_arbitrage", reason }).useVisible, false);
    }
  });

  it("waits for the first plan before style, export and Manual…", () => {
    const preparing = planCardView({ mode: "planner_passive_arbitrage", running: true, hasPlan: false });
    assert.equal(preparing.styleVisible, false);
    assert.equal(preparing.manualVisible, false);
    const planned = planCardView({ mode: "planner_passive_arbitrage", running: true, hasPlan: true });
    assert.equal(planned.styleVisible, true);
    assert.equal(planned.manualVisible, true);
  });

  it("offers a home without a battery no battery choices", () => {
    const view = planCardView({ mode: "planner_passive_arbitrage", running: true, hasPlan: true, hasBattery: false });
    assert.equal(view.styleVisible, false);
    assert.equal(view.exportVisible, false);
    assert.equal(view.manualVisible, false);
    // A manual mode someone picked still shows, so it can be changed back.
    assert.equal(planCardView({ mode: "idle", hasPlan: true, hasBattery: false }).manualVisible, true);
  });

  it("keeps a chosen manual mode's buttons in reach", () => {
    for (const mode of ["idle", "peak_shaving", "charge"]) {
      assert.equal(planCardView({ mode, running: false, reason: "planner-disabled" }).manualVisible, true, mode);
    }
  });

  it("shows nothing before the box has said its mode", () => {
    const view = planCardView({ mode: "", running: false, reason: "planner-disabled" });
    assert.equal(view.useVisible, false);
    assert.equal(view.manualVisible, false);
  });
});

describe("Use the plan", () => {
  it("turns the planner on through the Settings write, then restarts", async () => {
    const box = offBox();
    const said = [];
    const outcome = await startPlan({ api: box.api, say: (t) => said.push(t), running: false, reason: "planner-disabled" });
    assert.equal(outcome, "restart");
    const write = box.calls.find((c) => c.method === "POST" && c.path === "/api/config");
    // The whole document goes back as read, with the planner on, guarded by
    // the same ETag Settings uses.
    assert.deepEqual(write.body, { site: { name: "Home" }, price: { zone: "SE3" }, planner: { enabled: true } });
    assert.equal(write.headers["If-Match"], '"r7"');
    const order = box.calls.filter((c) => c.method === "POST").map((c) => c.path);
    assert.deepEqual(order, ["/api/config", "/api/planner/prefs", "/api/mode", "/api/restart"]);
    assert.deepEqual(said, ["Turning the planner on…", "Restarting FTW to start the planner…"]);
  });

  it("keeps the rest of an existing planner block", async () => {
    const box = offBox({
      "GET /api/config": { json: { planner: { enabled: false, soc_min: 0.2, engine: "energyplan" } } },
    });
    await startPlan({ api: box.api, running: false, reason: "planner-disabled" });
    const write = box.calls.find((c) => c.method === "POST" && c.path === "/api/config");
    assert.deepEqual(write.body.planner, { enabled: true, soc_min: 0.2, engine: "energyplan" });
  });

  it("does not ask a box that never sold whether selling may continue", async () => {
    const box = offBox();
    await startPlan({ api: box.api, running: false, reason: "planner-disabled" });
    const prefs = box.calls.find((c) => c.method === "POST" && c.path === "/api/planner/prefs");
    assert.deepEqual(prefs.body, { battery_export: "not_allowed" });
  });

  it("leaves an unknown answer alone when the planner already ran", async () => {
    const box = offBox();
    const outcome = await startPlan({ api: box.api, running: true });
    assert.equal(outcome, "mode");
    assert.equal(box.calls.some((c) => c.path === "/api/config"), false);
    assert.equal(box.calls.some((c) => c.method === "POST" && c.path === "/api/planner/prefs"), false);
    assert.equal(box.calls.some((c) => c.path === "/api/restart"), false);
  });

  it("takes the planner mode from the household's prefs", async () => {
    const box = offBox({
      "GET /api/planner/prefs": { json: { battery_export: "allowed", mapped_mode: "planner_" + "arbitrage" } },
    });
    await startPlan({ api: box.api, running: true });
    const mode = box.calls.find((c) => c.path === "/api/mode");
    assert.equal(mode.body.mode, "planner_" + "arbitrage");
  });

  it("falls back to the passive planner when prefs cannot be read", async () => {
    const box = offBox({ "GET /api/planner/prefs": { status: 500, json: { error: "down" } } });
    await startPlan({ api: box.api, running: true });
    assert.equal(box.calls.find((c) => c.path === "/api/mode").body.mode, PLANNER_FALLBACK_MODE);
  });

  it("restarts a planner that is on in Settings but not started", async () => {
    const box = offBox({ "GET /api/config": { json: { planner: { enabled: true } } } });
    const outcome = await startPlan({ api: box.api, running: false, reason: "" });
    assert.equal(outcome, "restart");
    assert.equal(box.calls.some((c) => c.method === "POST" && c.path === "/api/config"), false);
    // The planner was on, so an unknown answer may be a real upgrade question.
    assert.equal(box.calls.some((c) => c.method === "POST" && c.path === "/api/planner/prefs"), false);
  });

  it("stops before the mode when the box cannot plan, and says why", async () => {
    const box = offBox({ "GET /api/mpc/plan": { json: { enabled: false, reason: "no-price-provider" } } });
    await assert.rejects(
      startPlan({ api: box.api, running: false, reason: "planner-disabled" }),
      (err) => err.reason === "no-price-provider",
    );
    assert.equal(box.calls.some((c) => c.path === "/api/mode"), false);
    assert.equal(box.calls.some((c) => c.path === "/api/restart"), false);
  });

  it("shows the box's error when the write is refused", async () => {
    const box = offBox({
      "POST /api/config": { status: 409, json: { error: "Settings changed. Close and reopen Settings before saving." } },
    });
    await assert.rejects(
      startPlan({ api: box.api, running: false, reason: "planner-disabled" }),
      /Settings changed/,
    );
    assert.equal(box.calls.some((c) => c.path === "/api/mode"), false);
  });
});

describe("waiting for the restarted planner", () => {
  const clock = () => {
    let t = 0;
    return { now: () => t, sleep: async (ms) => { t += ms; } };
  };

  it("waits through the restart until the planner runs", async () => {
    const answers = [new Error("down"), { enabled: false }, { enabled: true }];
    const api = async () => {
      const next = answers.shift();
      if (next instanceof Error) throw next;
      return { ok: true, json: async () => next };
    };
    const c = clock();
    assert.deepEqual(await waitForPlanner({ api, ...c }), { running: true });
  });

  it("reports why a restarted box still cannot plan", async () => {
    const api = async () => ({ ok: true, json: async () => ({ enabled: false, reason: "no-battery-capacity" }) });
    assert.deepEqual(await waitForPlanner({ api, ...clock() }), { running: false, reason: "no-battery-capacity" });
  });

  it("gives up with a plain message", async () => {
    const api = async () => { throw new Error("down"); };
    await assert.rejects(waitForPlanner({ api, ...clock(), timeoutMs: 5000 }), /has not come back yet/);
  });
});
