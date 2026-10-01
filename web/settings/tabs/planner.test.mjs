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
const { styleForK } = await import("../../plan-prefs.js");
const tab = globalThis.window.FTWSettings.tabs.planner;
const { strategyLabel, styleNote, formatK, engineSelect } = tab._pure;

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

describe("styleNote", () => {
  it("names the Plan card style a fine-tuned margin belongs to", () => {
    assert.equal(styleNote(0.3, { styleForK }), "Balanced. Changes apply at once.");
    assert.equal(styleNote("0.15", { styleForK }), "Bold. Changes apply at once.");
    assert.equal(styleNote(0.7, { styleForK }), "Between two styles, nearest Careful. Changes apply at once.");
  });

  it("stays plain without the Plan card's style table", () => {
    assert.equal(styleNote(0.3, null), "Changes apply at once.");
    assert.equal(formatK("0.30000000000000004"), "0.3");
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

  it("renders the active-strategy placeholder and the live forecast margin above the engine details", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes('id="planner-active-strategy"'));
    const margin = html.indexOf('id="planner-style-k"');
    assert.ok(margin > 0 && margin < html.indexOf("<details"));
    assert.match(html, /<input type="range" id="planner-style-k" min="0" max="2" step="0\.05"/);
    assert.ok(html.includes('id="planner-margin-line"'));
    // The margin saves through /api/planner/prefs, never through config Save.
    assert.doesNotMatch(html, /data-path="planner\.pv_forecast_safety_k"/);
  });

  it("puts enabled, house reserve, and soc_max above a closed engine disclosure", () => {
    const html = tab.render(stubCtx());
    const detailsAt = html.indexOf("<details");
    assert.ok(detailsAt > 0);
    const top = html.slice(0, detailsAt);
    const rest = html.slice(detailsAt);
    assert.ok(top.includes('data-checkbox-path="planner.enabled"'));
    assert.ok(top.includes("[field:planner.soc_min]"));
    assert.ok(top.includes("[field:planner.soc_max]"));
    assert.ok(!top.includes('data-path="planner.engine"'));
    assert.ok(!top.includes("CLARABEL"));
    assert.ok(!top.includes("[select:planner.optimizer_solver]"));
    assert.match(rest, /<details class="engine-details">/);
    assert.doesNotMatch(html, /<details[^>]*\sopen\b/);
    assert.ok(rest.includes("Engine controls — leave these unless you are debugging."));
    assert.ok(rest.includes('data-path="planner.engine"'));
    assert.ok(!rest.includes("planner.optimizer_"));
    assert.ok(rest.includes("Energyplan uses a 500 ms solve limit"));
  });

  it("does not bind pv_forecast_safety_k when YAML left it unset", () => {
    const html = tab.render(stubCtx());
    assert.ok(!html.includes("[field:planner.pv_forecast_safety_k]"));
    assert.ok(!html.includes("config.yaml sets pv_forecast_safety_k"));
  });

  it("says a YAML pv_forecast_safety_k only seeds the first start", () => {
    const ctx = stubCtx();
    ctx.config.planner = { pv_forecast_safety_k: 0.25 };
    const html = tab.render(ctx);
    const rest = html.slice(html.indexOf("<details"));
    assert.ok(!html.includes("[field:planner.pv_forecast_safety_k]"));
    assert.match(rest, /config\.yaml sets pv_forecast_safety_k to 0\.25\. It only seeds the first start/);
  });

  it("renders Energyplan controls without retired Python settings", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes('data-path="planner.engine"'));
  });

  it("binds SoC bounds as 0–1 fractions", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes("[field:planner.soc_min]"));
    assert.ok(html.includes("[field:planner.soc_max]"));
    assert.ok(!html.includes("planner.soc_min_pct"));
    assert.ok(!html.includes("planner.soc_max_pct"));
  });

  it("shows Core's default bounds without adding a planner config", () => {
    const ctx = stubCtx();
    ctx.config = {};
    const defaults = {};
    ctx.field = (label, path, type, value) => { defaults[path] = value; return ""; };
    tab.render(ctx);
    assert.equal(defaults["planner.soc_min"], 0.1);
    assert.equal(defaults["planner.soc_max"], 0.95);
    assert.deepEqual(ctx.config, {});
  });

  it("promotes legacy soc_min_pct / soc_max_pct into 0–1 fields", () => {
    const ctx = stubCtx();
    ctx.config.planner = { soc_min_pct: 10, soc_max_pct: 90 };
    tab.render(ctx);
    assert.equal(ctx.config.planner.soc_min, 0.1);
    assert.equal(ctx.config.planner.soc_max, 0.9);
    assert.equal(ctx.config.planner.soc_min_pct, undefined);
    assert.equal(ctx.config.planner.soc_max_pct, undefined);
  });

  it("keeps an already-set soc_min over a leftover percent key", () => {
    const ctx = stubCtx();
    ctx.config.planner = { soc_min: 0.15, soc_min_pct: 10, soc_max: 0.92, soc_max_pct: 90 };
    tab.render(ctx);
    assert.equal(ctx.config.planner.soc_min, 0.15);
    assert.equal(ctx.config.planner.soc_max, 0.92);
  });
});

// Exercise the rendered values that captureCurrentTab saves, including old aliases.
describe("engine selection", () => {
  for (const engine of [undefined, null, "", " ", "core", "go", "dp", "python", "energyplan"]) {
    it("preserves the configured choice on save: " + String(engine), () => {
      const html = engineSelect(engine, () => "");
      const selected = [...html.matchAll(/<option value="([^"]*)" selected>/g)].map(m => m[1]);
      const expected = engine === "python" ? "energyplan" : ["go", "dp"].includes(engine) ? "core" : String(engine ?? "").trim();
      assert.deepEqual(selected, [expected]);
      assert.match(html, /value="energyplan"/);
      assert.match(html, /Automatic \(release default\)/);
    });
  }
});
