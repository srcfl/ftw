import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { derivePlanBrief } from "./plan-brief.js";

const now = new Date(2026, 6, 24, 10, 7, 0, 0).getTime();
const slot = (offsetMinutes, overrides = {}) => ({
  slot_start_ms: now + offsetMinutes * 60_000,
  slot_len_min: 15,
  battery_w: 0,
  loadpoint_w: 0,
  pv_limit_w: 0,
  reason: "scheduled",
  confidence: 1,
  ...overrides,
});

describe("plan brief normalization", () => {
  it("describes planner-off operation as manual and omits battery copy when absent", () => {
    const brief = derivePlanBrief({
      enabled: false,
      plan: null,
      status: { mode: "self_consumption", drivers: {} },
      now,
    });

    assert.deepEqual(brief.state, {
      key: "manual",
      label: "Manual",
      tone: "idle",
    });
    assert.equal(brief.next.action, "Manual control is active");
    // Both manual sentences name the button that actually exists on the
    // card. There is no strategy picker to send anyone to any more.
    assert.match(brief.next.time, /Use the plan/);
    assert.equal(brief.soc, null);
  });

  it("keeps the current battery charge visible while the planner is off", () => {
    const brief = derivePlanBrief({
      enabled: false,
      plan: null,
      status: { mode: "idle", bat_soc: 0.72 },
      now,
    });

    assert.equal(brief.soc.label, "72% now");
    assert.match(brief.soc.detail, /active plan/);
  });

  it("uses a preparing state before the first validated schedule exists", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: null,
      status: { mode: "planner_passive_arbitrage", bat_soc: 0.44 },
      now,
    });

    assert.equal(brief.state.key, "preparing");
    assert.equal(brief.next.action, "Preparing the first plan");
    assert.match(brief.constraint, /No schedule/);
    assert.equal(brief.forecast.label, "Inputs pending");
  });

  it("chooses the live meaningful action for an active plan", () => {
    const plan = {
      actions: [
        slot(-7, { battery_w: 2400, soc: 0.48 }),
        slot(8, { battery_w: 0, soc: 0.49 }),
      ],
      solver: { engine: "cvxpy", backend: "osqp", status: "optimal" },
    };
    const brief = derivePlanBrief({
      enabled: true,
      plan,
      status: { mode: "planner_passive_arbitrage", bat_soc: 0.46 },
      now,
    });

    assert.equal(brief.state.key, "active");
    assert.equal(brief.next.action, "Charge battery at 2.4 kW");
    assert.match(brief.next.time, /^Now, until /);
    assert.equal(brief.reason, "Regular schedule refresh");
    assert.equal(brief.constraint, "No active safety adjustment");
    assert.equal(brief.soc.label, "48% after next step");
    assert.equal(brief.planner.label, "cvxpy / osqp");
  });

  it("marks a valid schedule ready when a manual mode is currently selected", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: { actions: [slot(8)], solver: {} },
      status: { mode: "idle" },
      now,
    });

    assert.equal(brief.state.key, "ready");
    assert.equal(brief.state.label, "Plan ready");
  });

  it("makes stale-plan fallback and its safety reason explicit", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: { actions: [slot(8)], solver: {} },
      status: { mode: "planner_arbitrage", plan_stale: true },
      now,
    });

    assert.deepEqual(brief.state, {
      key: "stale",
      label: "Fallback active",
      tone: "warn",
    });
    assert.match(brief.constraint, /schedule is old/);
  });

  it("shows the Core planner as an ordinary active plan, not a fallback", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: {
        actions: [slot(-7, { battery_w: 2400, soc: 0.48 }), slot(8)],
        solver: {
          engine: "core",
          backend: "dp",
          status: "optimal",
          soc_levels: 201,
          action_levels: 401,
        },
      },
      status: { mode: "planner_arbitrage", bat_soc: 0.46 },
      now,
    });

    assert.equal(brief.state.key, "active");
    assert.equal(brief.state.label, "Plan active");
    assert.equal(brief.planner.label, "core / dp");
    assert.equal(brief.planner.detail, "Plan result: Optimal");
  });

  it("names the built-in solver fallback without losing its reason", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: {
        actions: [slot(8)],
        solver: {
          fallback: true,
          fallback_reason: "worker unavailable",
        },
      },
      status: { mode: "planner_arbitrage" },
      now,
    });

    assert.equal(brief.state.key, "fallback");
    assert.equal(brief.state.label, "Built-in plan active");
    assert.equal(brief.planner.label, "Built-in fallback");
    assert.equal(brief.planner.detail, "Worker unavailable");
  });

  it("surfaces active safety clamps and modeled forecast periods", () => {
    const brief = derivePlanBrief({
      enabled: true,
      plan: {
        actions: [
          slot(8, { confidence: 0.8 }),
          slot(23, { confidence: 0.7 }),
        ],
        solver: {},
      },
      status: {
        mode: "planner_arbitrage",
        dispatch: [{ driver: "battery", clamped: true, target_w: 1800 }],
      },
      now,
    });

    assert.match(brief.constraint, /Safety adjusted battery to 1.8 kW/);
    assert.equal(brief.forecast.label, "Some modeled inputs");
    assert.match(brief.forecast.detail, /forecast after that/);
  });

  it("does not tell a user who already picked a planner mode to pick a strategy", () => {
    const brief = derivePlanBrief({
      enabled: false,
      unavailableReason: "no-battery-capacity",
      plan: null,
      status: { mode: "planner_passive_arbitrage", bat_soc: 0.4 },
      now,
    });

    assert.equal(brief.state.key, "blocked");
    assert.equal(brief.state.label, "Cannot plan");
    assert.match(brief.next.action, /controllable battery/);
    assert.match(brief.next.time, /Devices/);
    // A house with no controllable battery is not one button away from a
    // plan, so it must not be told to press one.
    assert.doesNotMatch(brief.next.time, /Use the plan/);
    assert.doesNotMatch(brief.planner.detail, /Use the plan/);
    assert.equal(brief.soc.label, "40% now");
  });

  it("names a missing price source instead of asking for another strategy click", () => {
    const brief = derivePlanBrief({
      enabled: false,
      unavailableReason: "no-price-provider",
      status: { mode: "planner_arbitrage" },
      now,
    });

    assert.equal(brief.state.key, "blocked");
    assert.match(brief.next.action, /electricity prices/);
    assert.match(brief.next.time, /Settings → Price/);
  });

  it("keeps the manual brief when a manual mode is selected and the planner is off", () => {
    const brief = derivePlanBrief({
      enabled: false,
      unavailableReason: "planner-disabled",
      status: { mode: "self_consumption" },
      now,
    });

    assert.equal(brief.state.key, "manual");
    assert.equal(brief.next.action, "Manual control is active");
  });
});
