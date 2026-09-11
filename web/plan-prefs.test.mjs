import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import {
  clampSafetyK,
  formatSafetyK,
  trustFromSafetyK,
  safetyK,
  hedgeLine,
  isBatterySale,
  exportSentence,
  prefsFromStatus,
  SAFETY_K_STEP,
} from "./plan-prefs.js";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

describe("safety k", () => {
  it("clamps to the slider's range and treats junk as balanced", () => {
    assert.equal(clampSafetyK(0), 0);
    assert.equal(clampSafetyK(0.85), 0.85);
    assert.equal(clampSafetyK("0.85"), 0.85);
    assert.equal(clampSafetyK(2), 2);
    assert.equal(clampSafetyK(2.5), 2);
    assert.equal(clampSafetyK(-1), 0);
    assert.equal(clampSafetyK("nope"), 1);
    assert.equal(clampSafetyK(undefined), 1);
  });

  it("keeps every 0.05 step a distinct position", () => {
    assert.equal(SAFETY_K_STEP, 0.05);
    const seen = new Set();
    for (let i = 0; i <= 40; i++) seen.add(formatSafetyK(i * SAFETY_K_STEP));
    assert.equal(seen.size, 41);
    assert.equal(formatSafetyK(0.85), "0.85");
    assert.equal(formatSafetyK(1), "1");
  });

  it("derives the legacy enum from k the way the box does", () => {
    assert.equal(trustFromSafetyK(0), "bold");
    assert.equal(trustFromSafetyK(0.25), "bold");
    assert.equal(trustFromSafetyK(0.3), "balanced");
    assert.equal(trustFromSafetyK(0.85), "balanced");
    assert.equal(trustFromSafetyK(1.5), "cautious");
    assert.equal(trustFromSafetyK(2), "cautious");
  });

  it("maps the legacy enum onto PV safety k 2 / 1 / 0", () => {
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

  it("follows a fractional k", () => {
    assert.equal(hedgeLine(0.85, 1891), "σ right now ≈ 1891 W → hedge = k·σ ≈ 1607 W");
    assert.equal(hedgeLine(0.9, 1891), "σ right now ≈ 1891 W → hedge = k·σ ≈ 1702 W");
  });

  it("names the per-slot share when the PV twin reports rel_mae", () => {
    assert.equal(
      hedgeLine(0.85, 1891, 0.25),
      "σ right now ≈ 1891 W → hedge = k·σ ≈ 1607 W · holds back 21% of each sunny slot",
    );
    // The per-slot haircut can exceed a whole slot; the line does not.
    assert.match(hedgeLine(2, 1891, 0.8), /holds back 100% of each sunny slot$/);
    // No rel_mae → the watt line alone, unchanged.
    assert.equal(hedgeLine(1, 432.16, 0), "σ right now ≈ 432 W → hedge = k·σ ≈ 432 W");
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
    assert.equal(p.safety_k, 1);
  });

  it("reads safety_k — the slider owns the number (#1017, #1020)", () => {
    const p = prefsFromStatus({
      forecast_trust: "balanced",
      battery_export: "allowed",
      safety_k: 0.85,
      planner_mapped_k: 0.85,
    });
    assert.equal(p.forecast_trust, "balanced");
    assert.equal(p.battery_export, "allowed");
    assert.equal(p.safety_k, 0.85);
  });

  it("falls back to planner_mapped_k, then to the enum, on an older box", () => {
    assert.equal(prefsFromStatus({ forecast_trust: "bold", planner_mapped_k: 0 }).safety_k, 0);
    assert.equal(prefsFromStatus({ forecast_trust: "cautious" }).safety_k, 2);
    assert.equal(prefsFromStatus({ forecast_trust: "bold" }).safety_k, 0);
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
      /Left follows the forecast fully — if it is right, that earns more\. Right keeps more in the battery in case the sun misses, closer to using the battery only for the house\./,
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
    assert.match(plan, /postPlannerPrefs\(clampSafetyK\(slider\.value\), p\.battery_export\)/);
    assert.doesNotMatch(html + plan + app, /\brisk\b/i);
  });

  it("gives the slider 41 positions carrying k itself, not three steps", () => {
    const tag = html.match(/<input type="range" id="forecast-trust-slider"[^>]*>/);
    assert.ok(tag, "slider input not found");
    assert.match(tag[0], /min="0"/);
    assert.match(tag[0], /max="2"/);
    assert.match(tag[0], /step="0\.05"/);
    assert.match(html, /id="forecast-trust-value"/);
  });

  it("POSTs safety_k and marks the plan replanning until the new plan lands", () => {
    assert.match(plan, /safety_k: clampSafetyK\(k\)/);
    assert.match(plan, /setReplanPending\(true\)/);
    assert.match(plan, /setReplanPending\(false\)/);
    assert.match(plan, /Replanning…/);
  });
});
