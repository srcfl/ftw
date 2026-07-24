import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const router = readFileSync(join(webRoot, "diagnose.js"), "utf8");
const plan = readFileSync(join(webRoot, "plan.js"), "utf8");

const destinations = ["overview", "energy", "plan", "history", "more"];

describe("dashboard information architecture", () => {
  it("exposes the same five destinations on desktop and mobile", () => {
    const desktop = html.match(/<nav class="app-tabs"[\s\S]*?<\/nav>/)?.[0] || "";
    const mobile = html.match(/<nav class="mobile-destinations"[\s\S]*?<\/nav>/)?.[0] || "";

    for (const destination of destinations) {
      assert.match(desktop, new RegExp(`data-view="${destination}"`));
      assert.match(mobile, new RegExp(`data-view="${destination}"`));
      assert.match(html, new RegExp(`id="view-${destination}"`));
    }
    assert.equal((desktop.match(/data-view=/g) || []).length, 5);
    assert.equal((mobile.match(/data-view=/g) || []).length, 5);
  });

  it("keeps route state accessible and historical deep links compatible", () => {
    assert.match(html, /role="tablist" aria-label="Primary navigation"/);
    assert.match(html, /aria-controls="view-overview" aria-selected="true"/);
    assert.match(router, /parts\[0\] === 'live' \? 'overview'/);
    assert.match(router, /parts\[0\] === 'diagnose' \? 'history'/);
    assert.match(router, /\['ArrowLeft', 'ArrowRight', 'Home', 'End'\]/);
  });

  it("moves each existing dashboard section instead of cloning it", () => {
    assert.match(router, /append\(plan, '#plan-section'\)/);
    assert.match(router, /insertHistory\('\.history-row'\)/);
    assert.doesNotMatch(router, /insertHistory\('\.savings-row'\)/);
    assert.match(router, /'#chart-section'/);
    assert.match(router, /'#drivers-section'/);
  });

  it("keeps saved vs no PV or battery on Overview", () => {
    const overview = html.match(
      /<main id="view-overview"[\s\S]*?<main id="view-energy"/,
    )?.[0] || "";

    assert.match(overview, /<section class="savings-row">/);
    assert.equal(
      (overview.match(/<ftw-savings-card\b/g) || []).length,
      1,
    );
  });
});

describe("plain-language plan briefing", () => {
  it("covers plan state, next action, reason, forecast, SoC and planner source", () => {
    for (const id of [
      "plan-state-badge",
      "plan-next-action",
      "plan-main-reason",
      "plan-constraint",
      "plan-forecast-state",
      "plan-expected-soc",
      "plan-solver-state",
    ]) {
      assert.match(html, new RegExp(`id="${id}"`));
      assert.match(plan, new RegExp(`['"]${id}['"]`));
    }
    assert.match(plan, /Fallback active/);
    assert.match(plan, /No active safety adjustment/);
    assert.match(plan, /forecast after that/);
    assert.match(plan, /at the end of the plan/);
  });
});
