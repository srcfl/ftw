import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const app = readFileSync(join(webRoot, "app.js"), "utf8");

describe("strategy mode picker", () => {
  it("keeps Manual… on the Plan card in simple view", () => {
    // The simple/advanced UI toggle hides diagnostics. Manual fallbacks
    // used to ride that same class, so a house already on Self (manual)
    // had no selected button and no labelled way back to a planner
    // strategy until someone found ★ Advanced.
    const strategy = html.match(/class="plan-strategy"[\s\S]*?class="plan-help"/)?.[0] || "";
    assert.match(strategy, /id="mode-advanced-btn"/);
    assert.match(strategy, /id="mode-buttons"/);
    assert.doesNotMatch(strategy, /class="[^"]*advanced-only/);
  });

  it("opens the manual drawer when the live mode lives there", () => {
    assert.match(app, /function revealManualModes\(mode\)/);
    assert.match(app, /revealManualModes\(activeMode\)/);
    assert.match(app, /revealManualModes\(currentMode\)/);
  });

  it("auto-opens only when the mode changes", () => {
    // The status poll repeats the same mode every couple of seconds. Without
    // the early return, each one would force the drawer back open and undo
    // "Hide manual" a second after the user pressed it.
    assert.match(app, /lastRevealedMode = null/);
    assert.match(app, /if \(!mode \|\| mode === lastRevealedMode\) return;/);
  });

  it("offers one way back to the planner on the Plan card", () => {
    // Household prefs replaced Passive/Active as the primary buttons, so a
    // house already in a manual mode had nothing left to press to start
    // planning again.
    const strategy = html.match(/class="plan-strategy"[\s\S]*?class="plan-help"/)?.[0] || "";
    assert.match(strategy, /id="plan-use-btn"/);
    assert.match(strategy, /Use the plan/);
  });

  it("shows Use the plan only while the planner is not driving", () => {
    assert.match(app, /var plannerActive = \(data\.mode \|\| ""\)\.indexOf\("planner_"\) === 0;/);
    assert.match(app, /planUseBtn\.hidden = plannerActive;/);
    assert.match(app, /planUseRow\.hidden = plannerActive;/);
  });

  it("takes the planner mode from the household's own prefs", () => {
    // The server maps prefs to a planner key; reading mapped_mode keeps the
    // dashboard from deciding whether this battery may sell.
    assert.match(app, /apiFetch\("\/api\/planner\/prefs"/);
    assert.match(app, /var mapped = prefs && prefs\.mapped_mode;/);
    assert.match(app, /setMode\(typeof mapped === "string"/);
  });

  it("falls back to the passive planner, never to selling", () => {
    assert.match(app, /var PLANNER_FALLBACK_MODE = "planner_passive_arbitrage";/);
    // Both the unusable answer and the failed read take that fallback.
    assert.match(app, /\?\s*mapped\s*:\s*PLANNER_FALLBACK_MODE\);/);
    assert.match(app, /\.catch\(function \(\) \{\s*setMode\(PLANNER_FALLBACK_MODE\);/);
    // Whatever else the file grows, no path here may name the exporting
    // mode outright: permission to sell comes from the household, through
    // mapped_mode, or not at all.
    const useBlock = app.slice(app.indexOf("PLANNER_FALLBACK_MODE"), app.indexOf("mode-advanced-btn"));
    assert.doesNotMatch(useBlock, /"planner_arbitrage"/);
  });

  it("marks the tapped mode before the POST returns", () => {
    assert.match(app, /function markModeActive\(mode\)/);
    assert.match(
      app,
      /function setMode\(mode\) \{\s*markModeActive\(mode\);\s*revealManualModes\(mode\);/s,
    );
    // …and holds it, so a status read already in flight with the old mode
    // can't flash the previous button back.
    assert.match(app, /pendingMode = mode;\s*pendingModeUntil = Date\.now\(\)/);
    assert.match(app, /var activeMode = pendingMode \|\| data\.mode;/);
  });
});
