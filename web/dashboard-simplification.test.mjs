import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const app = readFileSync(join(webRoot, "app.js"), "utf8");
const css = readFileSync(join(webRoot, "app.css"), "utf8");
const plan = readFileSync(join(webRoot, "plan.js"), "utf8");
const router = readFileSync(join(webRoot, "diagnose.js"), "utf8");
const flow = readFileSync(join(webRoot, "components/ftw-energy-flow.js"), "utf8");
const price = readFileSync(join(webRoot, "components/ftw-price-chart.js"), "utf8");
const savings = readFileSync(join(webRoot, "components/ftw-savings-card.js"), "utf8");
const overview = html.match(/<main id="view-overview"[\s\S]*?<\/main>/)?.[0] || "";
const history = html.match(/<main id="view-history"[\s\S]*?<\/main>/)?.[0] || "";

describe("simplified dashboard overview", () => {
  it("answers now, price, plan, today, and fuse in that order", () => {
    for (const id of [
      "charging-notices",
      "power-now",
      "overview-price",
      "overview-plan-summary",
      "overview-today",
      "card-fuse",
    ]) {
      assert.match(overview, new RegExp(`id="${id}"`));
    }
    assert.ok(
      overview.indexOf('id="charging-notices"') <
        overview.indexOf('id="power-now"'),
      "the plugged-in car notice should precede Power now",
    );

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

  it("spans the plugged-in car notice across the Overview grid", () => {
    // #view-overview is 12 columns. A direct child without a span occupies
    // one column and wraps the notice a word per line, leaving the rest of
    // the row empty (v3.1.3-beta.1 field report).
    assert.match(
      css,
      /body\.ftw-app #charging-notices\s*\{[^}]*grid-column:\s*1\s*\/\s*-1/,
    );
    assert.match(css, /body\.ftw-app #charging-notices\[hidden\]/);
    assert.match(app, /getElementById\("charging-notices"\)/);
  });

  it("offers accessible Flow and Values panels around the existing diagram", () => {
    assert.match(overview, /role="tablist" aria-label="Power now view"/);
    assert.match(
      overview,
      /id="power-now-tab-flow"[\s\S]*?data-power-now-mode="flow"[\s\S]*?aria-controls="power-now-flow"/,
    );
    assert.match(
      overview,
      /id="power-now-tab-values"[\s\S]*?data-power-now-mode="values"[\s\S]*?aria-controls="power-now-values"/,
    );
    assert.ok(
      overview.indexOf('id="power-now-tab-flow"') <
        overview.indexOf('id="power-now-tab-values"'),
    );
    assert.match(
      overview,
      /id="power-now-tab-flow"[^>]*aria-selected="true"/,
    );
    assert.match(
      overview,
      /id="power-now-flow"[^>]*role="tabpanel"(?![^>]*hidden)/,
    );
    assert.match(
      overview,
      /id="power-now-values"[^>]*role="tabpanel"[^>]*hidden/,
    );
    assert.match(
      css,
      /body\.ftw-app\.mode-numbers \.power-now-tabs::before\s*\{[^}]*transform:\s*translateX\(100%\)/,
    );
    assert.doesNotMatch(
      css,
      /body\.ftw-app\.mode-hero \.power-now-tabs::before\s*\{[^}]*translateX\(100%\)/,
    );
    assert.match(overview, /<ftw-energy-flow id="energy-flow"[^>]*embedded/);
    assert.match(flow, /:host\(\[embedded\]\) \.title/);
  });

  it("replays the first live payload when the Flow mapper or component becomes ready", () => {
    assert.match(app, /lastFlowStatus/);
    assert.match(app, /lastFlowReadings/);
    assert.match(app, /ftwOnFlowMapperReady/);
    assert.match(
      app,
      /customElements\.whenDefined\("ftw-energy-flow"\)[\s\S]*?paintEnergyFlow\(lastFlowStatus\)/,
    );
    assert.match(app, /setReadings\(lastFlowReadings\)/);
  });

  it("builds the hero from the shared status mapper, not inline 0 W defaults", () => {
    assert.match(app, /ftwFlowReadingsFromStatus/);
    assert.doesNotMatch(app, /var gkw = \(data\.grid_w \|\| 0\) \/ 1000/);
  });

  it("does not feed the live stats strip 0 W when configured solar or battery is offline", () => {
    assert.match(app, /updateLiveStat\("pv", pvStat/);
    assert.match(app, /pvConfigured && !pvLive \? null/);
    assert.match(app, /updateLiveStat\("bat", batStat/);
    assert.match(app, /batConfigured && !batLive \? null/);
    assert.match(app, /updateLiveSocStat\(socStat\)/);
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

  it("adds compact daily energy and monetary savings periods without another status poller", () => {
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
    assert.match(savings, /return SAVINGS_LOOKBACK_DAYS/);
    for (const role of ["compact-today", "compact-week", "compact-month"]) {
      assert.match(savings, new RegExp(`data-role=["']${role}["']`));
    }
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
    // The toggle now chooses consumer total vs raw spot, not VAT on/off, so
    // the sync event is named for what it carries.
    assert.match(price, /ftw-price-mode-change/);
    assert.match(price, /detail: \{ totalOn: next \}/);
    assert.match(price, /href="#energy"/);
  });

  it("sizes the compact card from its own width, not the window's", () => {
    // On Overview this card sits in a column much narrower than the
    // viewport, so a @media breakpoint never fired for it: the headline was
    // set from 5vw, hit its ceiling, and ran through the cheapest-window
    // column beside it.
    assert.match(price, /:host\(\[compact\]\) \{ container-type: inline-size; \}/);
    assert.match(price, /font-size: clamp\([^)]*cqi[^)]*\)/);
    assert.match(price, /@container \(max-width: 400px\)/);
    // The window label wraps instead of truncating — "Tomorrow 11:00–13…"
    // drops the end of the window, which is half the answer.
    assert.doesNotMatch(price, /\.compact-low small \{[^}]*text-overflow: ellipsis/s);
    // No compact sizing left behind in the viewport breakpoint.
    const mediaBlock = price.match(/@media \(max-width: 600px\) \{[\s\S]*?\n    \}/)?.[0] || "";
    assert.doesNotMatch(mediaBlock, /\.compact-summary|\.compact-value|\.compact-low/);
  });

  it("renders Overview and Plan from the sole plan polling path", () => {
    // The cache-bust token moves whenever plan.js changes; what this test
    // is about is that the page loads plan.js as a module at all.
    assert.match(html, /<script type="module" src="\/plan\.js(\?v=[^"]*)?"><\/script>/);
    assert.match(plan, /import \{ derivePlanBrief, unavailablePlannerCopy \} from "\.\/plan-brief\.js"/);
    assert.equal((plan.match(/apiFetch\(['"]\/api\/mpc\/plan['"]/g) || []).length, 1);
    assert.doesNotMatch(app, /apiFetch\(['"]\/api\/mpc\/plan['"]/);
    assert.match(plan, /ftw-plan-data/);
    assert.match(app, /ftw-plan-data/);
    for (const id of [
      "overview-plan-state",
      "overview-plan-action",
      "overview-plan-time",
      "overview-plan-reason",
      "overview-plan-constraint",
      "overview-plan-soc",
    ]) {
      assert.match(plan, new RegExp(`["']${id}["']`));
    }
  });

  it("keeps the daily story primary and technical evidence contextual", () => {
    assert.match(
      router,
      /\['#chart-section', '\.energy-row', '\.prices-row', '#heating-section'\]/,
    );
    assert.match(router, /append\(plan, '#plan-history-details'\)/);
    assert.match(history, /class="energy-history-story"/);
    assert.match(history, /id="energy-history-details"/);
    assert.match(history, /id="plan-history-details"/);
    assert.ok(
      history.indexOf('class="energy-history-story"') <
        history.indexOf('id="energy-history-details"'),
      "the daily story should precede raw source records",
    );
  });

  it("keeps the mobile primary answers above the fixed destination bar", () => {
    assert.match(
      css,
      /\.overview-heading > div:first-child > p:last-child \{\s*display: none;/,
    );
    assert.match(
      css,
      /\.summary-card \{[\s\S]*?min-height: 44px;[\s\S]*?padding: 3px 10px;/,
    );
    assert.match(
      price,
      /@media \(max-width: 600px\)[\s\S]*?\.compact-profile \{\s*height: 44px;\s*margin-top: 10px;/,
    );
    assert.match(
      flow,
      /@media \(max-width: 600px\)[\s\S]*?:host\(\[embedded\]\) \.ef-toggle \{\s*bottom: 30px;/,
    );
  });
});
