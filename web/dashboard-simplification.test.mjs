import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const app = readFileSync(join(webRoot, "app.js"), "utf8");
const flow = readFileSync(join(webRoot, "components/ftw-energy-flow.js"), "utf8");
const price = readFileSync(join(webRoot, "components/ftw-price-chart.js"), "utf8");
const savings = readFileSync(join(webRoot, "components/ftw-savings-card.js"), "utf8");
const overview = html.match(/<main id="view-overview"[\s\S]*?<\/main>/)?.[0] || "";

describe("simplified dashboard overview", () => {
  it("answers now, price, plan, today, and fuse in that order", () => {
    for (const id of [
      "power-now",
      "overview-price",
      "overview-plan-summary",
      "overview-today",
      "card-fuse",
    ]) {
      assert.match(overview, new RegExp(`id="${id}"`));
    }

    const orderedIds = [
      "power-now",
      "overview-price",
      "overview-plan-summary",
      "overview-today",
      "card-fuse",
    ];
    for (let index = 1; index < orderedIds.length; index += 1) {
      assert.ok(
        overview.indexOf(`id="${orderedIds[index - 1]}"`) <
          overview.indexOf(`id="${orderedIds[index]}"`),
        `${orderedIds[index - 1]} should precede ${orderedIds[index]}`,
      );
    }
  });

  it("offers accessible Values and Flow panels around the existing diagram", () => {
    assert.match(overview, /role="tablist" aria-label="Power now view"/);
    assert.match(
      overview,
      /id="power-now-tab-values"[\s\S]*?data-power-now-mode="values"[\s\S]*?aria-controls="power-now-values"/,
    );
    assert.match(
      overview,
      /id="power-now-tab-flow"[\s\S]*?data-power-now-mode="flow"[\s\S]*?aria-controls="power-now-flow"/,
    );
    assert.match(overview, /id="power-now-values"[^>]*role="tabpanel"/);
    assert.match(overview, /id="power-now-flow"[^>]*role="tabpanel"[^>]*hidden/);
    assert.match(overview, /<ftw-energy-flow id="energy-flow"[^>]*embedded/);
    assert.match(flow, /:host\(\[embedded\]\) \.title/);
  });

  it("keeps each live telemetry rendering target singular", () => {
    for (const id of [
      "grid-w",
      "pv-w",
      "load-w",
      "bat-w",
      "card-ev-w",
      "energy-flow",
    ]) {
      assert.equal(
        (html.match(new RegExp(`id="${id}"`, "g")) || []).length,
        1,
        `${id} must occur exactly once`,
      );
    }
  });

  it("adds compact daily readings without another status poller", () => {
    for (const id of [
      "bat-soc",
      "overview-e-import",
      "overview-e-export",
      "overview-e-pv",
    ]) {
      assert.match(overview, new RegExp(`id="${id}"`));
      assert.match(app, new RegExp(`["']${id}["']`));
    }
    assert.match(overview, /<ftw-savings-card[^>]*compact/);
    assert.match(savings, /observedAttributes[\s\S]*compact/);
    assert.doesNotMatch(app, /fetch\(['"]\/api\/mpc\/plan/);
  });

  it("keeps compact and detailed price views on one preference contract", () => {
    assert.match(overview, /<ftw-price-chart compact><\/ftw-price-chart>/);
    assert.equal((html.match(/<ftw-price-chart/g) || []).length, 2);
    assert.match(
      html,
      /<section class="prices-row">[\s\S]*?<ftw-price-chart><\/ftw-price-chart>/,
    );
    assert.match(price, /observedAttributes[\s\S]*compact/);
    assert.match(price, /buildCompactPriceView/);
    assert.match(price, /ftw-price-vat-change/);
    assert.match(price, /href="#energy"/);
  });
});
