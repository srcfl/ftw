import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import {
  sliderFromTrust,
  trustFromSlider,
  safetyK,
  hedgeLine,
  isBatterySale,
  exportSentence,
  prefsFromStatus,
} from "./plan-prefs.js";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

describe("forecast trust mapping", () => {
  it("maps the three slider steps onto cautious / balanced / bold", () => {
    assert.equal(trustFromSlider(0), "cautious");
    assert.equal(trustFromSlider(1), "balanced");
    assert.equal(trustFromSlider(2), "bold");
    assert.equal(trustFromSlider("1"), "balanced");
    assert.equal(sliderFromTrust("cautious"), 0);
    assert.equal(sliderFromTrust("balanced"), 1);
    assert.equal(sliderFromTrust("bold"), 2);
    assert.equal(sliderFromTrust("nope"), 1);
  });

  it("maps trust only onto PV safety k 2 / 1 / 0", () => {
    assert.equal(safetyK("cautious"), 2);
    assert.equal(safetyK("balanced"), 1);
    assert.equal(safetyK("bold"), 0);
  });
});

describe("hedge line", () => {
  it("formats k·σ in watts", () => {
    assert.equal(hedgeLine(1, 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 432 W");
    assert.equal(hedgeLine(2, 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 864 W");
    assert.equal(hedgeLine(0, 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 0 W");
  });

  it("hides when σ is missing", () => {
    assert.equal(hedgeLine(1, null), null);
    assert.equal(hedgeLine(1, -1), null);
  });
});

describe("export sentences", () => {
  const noon = Date.UTC(2026, 7, 21, 10, 0, 0);
  const slot = (start, battery_w, grid_w) => ({
    slot_start_ms: start,
    slot_len_min: 15,
    battery_w,
    grid_w,
  });

  it("names a planned battery sale window", () => {
    const actions = [
      slot(noon, -2000, -1500),
      slot(noon + 15 * 60_000, -1800, -1200),
    ];
    const text = exportSentence({ actions, exportPermission: "allowed", nowMs: noon });
    assert.match(text, /^Battery sale planned \d{2}:\d{2}–\d{2}:\d{2}\.$/);
  });

  it("reports solar export when the battery is not selling", () => {
    const actions = [slot(noon, 0, -800)];
    assert.equal(
      exportSentence({ actions, exportPermission: "allowed", nowMs: noon }),
      "Solar export only; the battery is not selling.",
    );
  });

  it("reports no worthwhile sale when export is allowed and nothing exports", () => {
    const actions = [slot(noon, 500, 200)];
    assert.equal(
      exportSentence({ actions, exportPermission: "allowed", nowMs: noon }),
      "Battery export is allowed, but FTW found no worthwhile sale.",
    );
  });

  it("reports a blocked sale when permission is off or unknown", () => {
    const actions = [slot(noon, 0, 100)];
    assert.equal(
      exportSentence({ actions, exportPermission: "not_allowed", nowMs: noon }),
      "Battery sale blocked: permission is off or not checked.",
    );
    assert.equal(
      exportSentence({ actions, exportPermission: "unknown", nowMs: noon }),
      "Battery sale blocked: permission is off or not checked.",
    );
  });

  it("does not treat house-only discharge as a battery sale", () => {
    assert.equal(isBatterySale({ battery_w: -2000, grid_w: 300 }), false);
    assert.equal(isBatterySale({ battery_w: -2000, grid_w: -400 }), true);
  });
});

describe("prefsFromStatus", () => {
  it("defaults to balanced + unknown", () => {
    const p = prefsFromStatus({});
    assert.equal(p.forecast_trust, "balanced");
    assert.equal(p.battery_export, "unknown");
    assert.equal(p.yaml_custom, false);
    assert.equal(p.mapped_k, 1);
  });

  it("passes through yaml_custom and mapped_k", () => {
    const p = prefsFromStatus({
      forecast_trust: "bold",
      battery_export: "allowed",
      planner_yaml_custom: true,
      planner_mapped_k: 0.25,
    });
    assert.equal(p.forecast_trust, "bold");
    assert.equal(p.battery_export, "allowed");
    assert.equal(p.yaml_custom, true);
    assert.equal(p.mapped_k, 0.25);
  });
});

describe("Plan card markup and wiring", () => {
  it("puts follow-the-forecast on the Plan card, not Passive/Active as primary", () => {
    assert.match(html, /id="forecast-trust-slider"/);
    assert.match(html, /Hold reserve/);
    assert.match(html, /Trust forecast/);
    assert.match(html, /Follow the forecast/);
    assert.match(html, /id="plan-export-check"/);
    assert.match(
      html,
      /Left keeps more in the battery if the sun might miss — closer to using the battery only for the house\. Right follows the forecast fully\. If the forecast is right, right earns more\./,
    );
    assert.match(
      html,
      /Allow the battery to sell to the grid when the plan expects a worthwhile sale\./,
    );
    assert.match(
      html,
      /Solar can still export when this is off\. Check your electricity contract\./,
    );
    assert.match(html, /Not checked — battery export stays off\./);
    assert.match(html, /FTW used to sell from the battery on high-price hours\. Allow that to continue\?/);
    assert.doesNotMatch(html, />Strategy</);
    assert.match(app, /String\(m\.key \|\| ""\)\.indexOf\("planner_"\) === 0\) return/);
    assert.match(plan, /\/api\/planner\/prefs/);
    assert.match(plan, /postPlannerPrefs\(trustFromSlider\(slider\.value\), p\.battery_export\)/);
    assert.doesNotMatch(html + plan + app, /\brisk\b/i);
  });
});
