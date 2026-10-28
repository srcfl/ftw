import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

const read = (path) => readFileSync(new URL(`./${path}`, import.meta.url), "utf8");

// One distinct value per role, so a draw call shows which role it used.
const ROLES = {
  text: "ink", dim: "ink2", muted: "ink3", line: "line", panel: "panel", accent: "ink",
  solar: "solar", stores: "stores", gives: "gives", grid: "grid", car: "car",
  live: "live", alarm: "alarm",
};

function recordingContext() {
  const calls = [];
  const texts = [];
  const ctx = {
    fillStyle: "", strokeStyle: "", globalAlpha: 1, lineWidth: 1, font: "", textAlign: "",
    fillRect(x, y, w, h) { calls.push({ fill: this.fillStyle, alpha: this.globalAlpha, x, y, w, h }); },
    fillText(text, x, y) { texts.push({ text, x, y }); },
  };
  for (const name of ["scale", "clearRect", "beginPath", "moveTo", "lineTo", "stroke", "save", "restore", "setLineDash", "setTransform"]) {
    ctx[name] = () => {};
  }
  return { ctx, calls, texts };
}

// Draw History's daily chart for these days and return what it drew.
async function drawHistory(days) {
  const { ctx, calls, texts } = recordingContext();
  const canvas = { clientWidth: 600, width: 0, height: 0, getContext: () => ctx };
  const legend = { innerHTML: "" };
  const els = { "energy-history-chart": canvas, "energy-history-legend": legend };
  const context = {
    document: { getElementById: (id) => els[id] || null },
    window: {
      devicePixelRatio: 1,
      ftwThemeColors: { palette: () => ({ ...ROLES }) },
      addEventListener() {},
    },
    fetch: async () => ({ ok: true, json: async () => ({ days }) }),
  };
  vm.createContext(context);
  vm.runInContext(read("energy-history.js"), context);
  await context.window.ftwEnergyHistoryLoad();
  await new Promise((resolve) => setImmediate(resolve));
  return { calls, texts, legend };
}
const day = (d, load, pv, imp, exp) => ({ day: d, load_wh: load, pv_wh: pv, import_wh: imp, export_wh: exp });

describe("charts draw in theme roles", () => {
  it("carries none of the old fixed chart hues", () => {
    // Red and green grid and prices, amber charge, purple discharge, blue
    // SoC, cyan car, and the heating chart's own hues.
    const legacy = /#(?:ef4444|22c55e|f59e0b|8b5cf6|60a5fa|86efac|fde68a|22d3ee|10b981|3b82f6|eab308|f97316|38bdf8|a78bfa|f472b6|fbbf24|0891b2|fdba74|facc15|67e8f9|7788ff|93c5fd)\b|rgba\(\s*(?:34,\s*197,\s*94|239,\s*68,\s*68|245,\s*158,\s*11|139,\s*92,\s*246|96,\s*165,\s*250|34,\s*211,\s*238|251,\s*191,\s*36|253,\s*230,\s*138|148,\s*163,\s*184|100,\s*116,\s*139|167,\s*139,\s*250|6,\s*182,\s*212|251,\s*146,\s*60|250,\s*204,\s*21)/gi;
    for (const path of ["plan.js", "energy-history.js", "diagnose.js", "heating.js", "app.js", "index.html", "style.css"]) {
      assert.deepEqual(read(path).match(legacy) || [], [], path);
    }
    assert.match(read("app.css"), /\.ev-timeline \{ --ev-plan-color: var\(--violet\);/);
  });

  it("draws plan prices as ink by level and the battery in its two hues", () => {
    const plan = read("plan.js");
    assert.match(plan, /PRICE_INK = \{ cheap: 0\.32, flat: 0\.5, dear: 0\.8/);
    assert.match(plan, /\{ ore: spotOre,\s+color: C\.text,\s+alpha: level/);
    assert.match(plan, /ctx\.fillStyle = a\.battery_w >= 0 \? C\.stores : C\.gives;/);
    assert.match(plan, /ctx\.strokeStyle = C\.car;/);
  });

  it("hangs grid export below the zero line, in the grid's one hue", async () => {
    const { calls, texts, legend } = await drawHistory([day("2026-10-24", 20000, 10000, 15000, 0), day("2026-10-25", 10000, 30000, 2000, 10000)]);

    // Four bars a day: home use, solar, import, export.
    assert.equal(calls.length, 8);
    assert.deepEqual(calls.map((bar) => bar.fill), ["ink", "solar", "grid", "grid", "ink", "solar", "grid", "grid"]);
    const [, , , , , solar, imported, exported] = calls;
    // Import stands on the zero line; export starts on it and goes down.
    const zeroY = imported.y + imported.h;
    assert.equal(exported.y, zeroY);
    assert.ok(exported.h > 0);
    assert.equal(solar.y + solar.h, zeroY);
    assert.match(legend.innerHTML, /background:grid"><\/i>Grid export, below the line/);
    assert.ok(texts.some((t) => t.text === "sold"), "the lower labels say what they count");
  });

  it("gives export its own gridlines when it outgrows every bar above zero", async () => {
    const { calls, texts } = await drawHistory([day("2026-10-24", 8000, 12000, 3000, 30000), day("2026-10-25", 6000, 9000, 2000, 18000)]);
    const exported = calls[3]; // the first day's 30 kWh export
    const lowestBar = Math.max(...calls.filter((c) => c.fill === "grid").map((c) => c.y + c.h));
    const labels = texts.filter((t) => / kWh$/.test(t.text));
    const lowest = labels.reduce((a, b) => (b.y > a.y ? b : a));
    // Labels sit 4 px under their gridline.
    assert.ok(lowest.y - 4 >= lowestBar - 1e-6, "the lowest gridline reaches the largest export");
    assert.ok(parseFloat(lowest.text) >= 30, lowest.text);
    assert.ok(exported.h > 0);
    for (const label of labels) assert.doesNotMatch(label.text, /[-−]/, label.text);
  });

  it("tells live export from import by its dash, in the one grid hue", () => {
    const app = read("app.js");
    assert.match(app, /e_export\),\s+color: C\.grid,\s+width: 2, dash: \[6, 4\], name: "Export", fill: false/);
    assert.match(app, /name: "Export",\s+data: chartHistory\.e_export,\s+color: C\.grid, dash: true/);
    assert.match(app, /\["var\(--green-e\)", "Export", true\]/);
  });

  it("maps the live chart's roles to the same tokens as the shared resolver", async () => {
    const { createThemeColors } = await import("./theme-runtime.js");
    const shared = createThemeColors((name) => name).palette();
    const app = read("app.js");
    const context = {
      document: {
        createElement: () => ({ style: {} }),
        body: { appendChild() {} },
        documentElement: { getAttribute: () => "dark" },
      },
      getComputedStyle: (el) => ({ color: el.style.color }),
    };
    vm.createContext(context);
    vm.runInContext(app.slice(app.indexOf("  var _colorProbe = null;"), app.indexOf("  function drawHoverOverlay(")), context);
    const live = Object.fromEntries(
      Object.entries(context.chartColors()).map(([role, value]) => [role, /^var\((--[a-z0-9-]+)/.exec(value)[1]]),
    );
    // The live chart calls the panel its surface; every other name matches.
    assert.equal(live.surface, shared.panel);
    for (const role of ["text", "dim", "muted", "line", "accent", "solar", "stores", "gives", "grid", "car", "live"]) {
      assert.equal(live[role], shared[role], role);
    }
  });

  it("gives every battery the storing hue and every car the car's, a step fainter each", () => {
    const app = read("app.js");
    assert.doesNotMatch(app, /BATTERY_PALETTE|EV_PALETTE/);
    const start = app.indexOf("  var SERIES_STEPS");
    const end = app.indexOf("  // A driver name → presentation label.");
    const hex = app.slice(app.indexOf("  function hexAlpha("), app.indexOf("  // Resolve a CSS custom property to a concrete"));
    const context = {
      chartBatteries: { sungrow: {}, ferroamp: {} },
      chartEVs: { easee: {} },
      chartColors: () => ({ stores: "rgb(24, 217, 255)", car: "rgb(182, 156, 255)" }),
    };
    vm.createContext(context);
    vm.runInContext(app.slice(start, end) + hex + app.slice(app.indexOf("  function evColor("), app.indexOf("  function evLabel(")), context);
    assert.equal(context.batteryColor("ferroamp"), "rgba(24,217,255,1)");
    assert.equal(context.batteryColor("sungrow"), "rgba(24,217,255,0.72)");
    assert.equal(context.evColor("easee"), "rgba(182,156,255,1)");
    assert.equal(context.batterySwatch("sungrow"), "color-mix(in srgb, var(--cyan) 72%, transparent)");
    assert.equal(context.evSwatch("easee"), "color-mix(in srgb, var(--violet) 100%, transparent)");
  });
});
