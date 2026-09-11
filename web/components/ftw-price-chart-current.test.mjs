import assert from "node:assert/strict";
import { it } from "node:test";

// Exercise the real header renderer without a DOM or network. The SVG body
// is outside these assertions; the slot selection and price display are not.
let PriceChart;
globalThis.HTMLElement = class {};
globalThis.customElements = { define(name, component) {
  if (name === "ftw-price-chart") PriceChart = component;
} };
await import("./ftw-price-chart.js");
delete globalThis.HTMLElement;
delete globalThis.customElements;

function at(day, hour, minute = 0) {
  return new Date(2026, 8, 7 + day, hour, minute).getTime();
}

function render(items, horizon = "today", totalOn = false) {
  const chart = Object.assign(Object.create(PriceChart.prototype), {
    _data: { zone: "SE4", items },
    _horizon: horizon,
    _totalOn: totalOn,
    _gridTariff: 70,
    _vatPct: 25,
    _currency: "SEK",
    hasAttribute: () => false,
    _renderChart: () => "",
  });
  return chart.render();
}

function nowValue(html) {
  const match = html.match(/<span class="meta-label">now<\/span> ([^<]*)<\/span>/);
  assert.ok(match, "header must state whether a current price is available");
  return match[1];
}

it("shows the current quarter, including the exact interval boundary", (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: at(0, 18, 15) });
  const items = [
    { tsMs: at(0, 18), lenMin: 15, spot: 100 },
    { tsMs: at(0, 18, 15), lenMin: 15, spot: 200 },
    { tsMs: at(0, 18, 30), lenMin: 15, spot: 300 },
  ];
  assert.equal(nowValue(render(items)), "200.0 öre");
});

it("does not label tomorrow's first price as now", (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: at(0, 18, 25) });
  const items = [
    { tsMs: at(0, 18, 15), lenMin: 15, spot: 200 },
    { tsMs: at(1, 0), lenMin: 15, spot: 20 },
  ];
  assert.equal(nowValue(render(items, "tomorrow")), "—");
});

it("leaves now unavailable in a gap or after the last interval", (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: at(0, 18, 25) });
  const past = { tsMs: at(0, 18), lenMin: 15, spot: 100 };
  const future = { tsMs: at(0, 18, 30), lenMin: 15, spot: 300 };
  assert.equal(nowValue(render([past, future])), "—");
  assert.equal(nowValue(render([past])), "—");
});

it("keeps hourly slots, tariff, VAT and supplied totals consistent", (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: at(0, 18, 25) });
  const hourly = { tsMs: at(0, 18), lenMin: 60, spot: 20 };
  assert.equal(nowValue(render([hourly], "today", true)), "112.5 öre");
  assert.equal(nowValue(render([{ ...hourly, total: 109.3 }], "today", true)), "109.3 öre");
  assert.equal(nowValue(render([{ ...hourly, total: 109.3 }])), "20.0 öre");
});
