// node --test web/settings/tabs/planner.test.mjs
//
// Pure-function tests for the Settings → Planner tab. The tab is a
// classic (non-module) script that attaches to window.FTWSettings, so
// stub a window object and dynamic-import the file; the helpers are
// reachable via the _pure escape hatch.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./planner.js");
const tab = globalThis.window.FTWSettings.tabs.planner;
const { strategyLabel, hedgeLine } = tab._pure;

describe("strategyLabel", () => {
  it("maps every planner mode via the local fallback", () => {
    assert.equal(strategyLabel("planner_passive_arbitrage", null), "Passive arbitrage");
    assert.equal(strategyLabel("planner_arbitrage", null), "Active arbitrage");
    assert.equal(strategyLabel("planner_self", null), "Self-consumption (planner, legacy)");
    assert.equal(strategyLabel("planner_cheap", null), "Cheap charge (planner, legacy)");
  });

  it("suffixes non-planner modes as manual", () => {
    assert.equal(
      strategyLabel("self_consumption", null),
      "Self consumption (manual — planner not dispatching)"
    );
    assert.equal(strategyLabel("idle", null), "Idle (manual — planner not dispatching)");
  });

  it("prefers the /api/modes catalog label when present", () => {
    const catalog = [{ key: "planner_passive_arbitrage", label: "Passive arbitrage (catalog)" }];
    assert.equal(strategyLabel("planner_passive_arbitrage", catalog), "Passive arbitrage (catalog)");
  });

  it("still suffixes manual modes when the catalog provides the label", () => {
    const catalog = [{ key: "idle", label: "Idle" }];
    assert.equal(strategyLabel("idle", catalog), "Idle (manual — planner not dispatching)");
  });

  it("returns a dash for missing mode", () => {
    assert.equal(strategyLabel(null, null), "—");
    assert.equal(strategyLabel("", null), "—");
  });
});

describe("hedgeLine", () => {
  it("formats a normal σ with the hedge product", () => {
    assert.equal(hedgeLine("1", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 432 W");
    assert.equal(hedgeLine("2", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 864 W");
  });

  it("treats empty or junk k as 0", () => {
    assert.equal(hedgeLine("", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 0 W");
    assert.equal(hedgeLine("abc", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 0 W");
  });

  it("reports no hedge when σ is ~0", () => {
    assert.equal(hedgeLine("1", 0), "σ right now ≈ 0 W — no hedge");
    assert.equal(hedgeLine("1", 0.4), "σ right now ≈ 0 W — no hedge");
  });

  it("returns null when σ is missing or invalid (line stays hidden)", () => {
    assert.equal(hedgeLine("1", null), null);
    assert.equal(hedgeLine("1", undefined), null);
    assert.equal(hedgeLine("1", NaN), null);
    assert.equal(hedgeLine("1", -5), null);
  });
});

describe("render", () => {
  function stubCtx() {
    return {
      config: { planner: {} },
      field: (label, path) => "[field:" + path + "]",
      selectField: (label, path) => "[select:" + path + "]",
      help: () => "[?]",
    };
  }

  it("no longer renders the planner.mode dropdown", () => {
    const html = tab.render(stubCtx());
    assert.ok(!html.includes("planner.mode"), "planner.mode must not be bound in the form");
  });

  it("renders the active-strategy placeholder and hedge line containers", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes('id="planner-active-strategy"'));
    assert.ok(html.includes('id="planner-hedge-line"'));
    assert.ok(html.includes("Set from the Plan card on the dashboard"));
  });

  it("renders mathematical optimizer controls", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes("[select:planner.engine]"));
    assert.ok(html.includes("[select:planner.optimizer_solver]"));
    assert.ok(html.includes("[select:planner.optimizer_formulation]"));
    assert.ok(html.includes("[field:planner.optimizer_timeout_s]"));
    assert.ok(html.includes("[field:planner.optimizer_cvar_weight]"));
    assert.ok(html.includes("[select:planner.optimizer_challenger_policy]"));
    assert.ok(html.includes("[field:planner.optimizer_recourse_non_anticipative_slots]"));
    assert.ok(html.includes("[field:planner.optimizer_multistage.scenario_limit]"));
    assert.ok(html.includes("[field:planner.optimizer_multistage.branch_interval_slots]"));
    assert.ok(html.includes("[field:planner.optimizer_multistage.near_horizon_slots]"));
    assert.ok(html.includes("[field:planner.optimizer_multistage.service_cvar_weight]"));
    assert.ok(html.includes('data-checkbox-path="planner.optimizer_recourse_shadow"'));
  });
});
