// node --test web/price-zone-picker.test.mjs
//
// The settings tab and the setup wizard are DOM-coupled scripts that can't
// be imported under `node --test` (the repo ships no DOM polyfill), so
// these are structural tests over the source — the same approach
// setup.test.mjs takes. What they lock in: the zone list comes from the
// API rather than a hand-kept list in the page, and the currency follows
// the country instead of staying Swedish.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const priceTab = readFileSync(join(webRoot, "settings", "tabs", "price.js"), "utf8");
const setupJs = readFileSync(join(webRoot, "setup.js"), "utf8");
const setupHtml = readFileSync(join(webRoot, "setup.html"), "utf8");
const chart = readFileSync(join(webRoot, "components", "ftw-price-chart.js"), "utf8");
const plan = readFileSync(join(webRoot, "plan.js"), "utf8");
const loadpoints = readFileSync(join(webRoot, "loadpoints.js"), "utf8");
const savings = readFileSync(join(webRoot, "components", "ftw-savings-card.js"), "utf8");

// Comments carry the history of the öre-only era; only emitted strings matter.
function withoutComments(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("\n")
    .filter((line) => !line.trim().startsWith("//"))
    .join("\n");
}

describe("price zone picker", () => {
  it("builds both lists from the zone API, not a list kept in the page", () => {
    assert.match(priceTab, /\/api\/prices\/zones/);
    assert.match(setupJs, /\/api\/prices\/zones/);
  });

  it("offers a country before a zone code — nobody knows Belgium is BE", () => {
    assert.match(priceTab, /id="price-country"/);
    assert.match(setupHtml, /id="price-country"/);
  });

  it("moves the currency with the country", () => {
    assert.match(priceTab, /setByPath\(ctx\.config, "price\.currency", first\.currency\)/);
    assert.match(setupJs, /currencyForZone/);
    assert.match(setupJs, /cfg\.price\.currency = cur/);
  });

  it("keeps elprisetjustnu on the Swedish zones it can actually serve", () => {
    assert.match(priceTab, /elprisetjustnu[\s\S]{0,200}indexOf\("SE"\)/);
  });

  it("survives an API that doesn't answer", () => {
    // Both surfaces ship a fallback list, so the tab is still usable.
    assert.match(priceTab, /FALLBACK/);
    assert.match(setupHtml, /<option value="SE3"/);
  });

  it("offers a static tariff for markets with no day-ahead feed", () => {
    assert.match(priceTab, /"static"/);
    assert.match(priceTab, /price\.static_ore_kwh/);
    assert.match(priceTab, /static_tou/);
    assert.match(setupHtml, /value="static"/);
  });
});

describe("price labels follow the currency", () => {
  it("leaves no hardcoded öre in the price chart or the plan chart", () => {
    for (const [name, src] of [["ftw-price-chart.js", chart], ["plan.js", plan]]) {
      assert.doesNotMatch(withoutComments(src), /öre/,
        `${name} still spells a price unit "öre" — it has to come from price-units.js`);
    }
  });

  it("labels the loadpoint schedule column from the configured currency", () => {
    assert.match(loadpoints, /priceColumnHeader/);
    assert.match(loadpoints, /unitLabel\(u\.activeCurrency\(\)\)/);
  });

  it("takes the currency from the price API, which owns what the numbers are in", () => {
    assert.match(chart, /setActiveCurrency\(j\.currency\)/);
    assert.match(plan, /setActiveCurrency\(\(p && p\.currency\)/);
  });

  it("reports savings and plan cost in the same currency the prices are in", () => {
    // "+247 SEK saved" over EUR numbers is the same bug as "öre" over cent.
    // A bare 'SEK' literal is the default an install without the setting
    // falls back to — it's a label glued to an amount that's wrong.
    const withoutDefaults = (src) => withoutComments(src).replace(/(['"])SEK\1/g, "");
    for (const [name, src] of [["ftw-savings-card.js", savings], ["plan.js", plan]]) {
      assert.doesNotMatch(withoutDefaults(src), /\bSEK\b/,
        `${name} still prints a hardcoded SEK amount`);
    }
  });
});
