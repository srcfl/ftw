import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

import "./power-words.js";
import { flowReadingsFromStatus } from "./components/energy-flow-readings.js";
import { derivePlanBrief } from "./plan-brief.js";

// FTW's rule: the UI never shows a raw minus. It shows a size, and a short
// word says the direction: import or export, charge or discharge. Home use
// keeps Core's sign for now (#1768); see formatSignedW.
const words = globalThis.ftwPowerWords;
const read = (path) => readFileSync(new URL(`./${path}`, import.meta.url), "utf8");

// One function's source, found by name and closed by matching braces, so a
// test does not hang on whatever happens to follow it in the file.
function fn(source, name) {
  const start = source.search(new RegExp(`function ${name}\\(`));
  assert.ok(start >= 0, `function ${name} not found`);
  return closed(source, start, source.indexOf("{", start));
}
function closed(source, start, open) {
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}" && --depth === 0) return source.slice(start, i + 1);
  }
  throw new Error("not closed");
}
function run(code, context = {}) {
  vm.createContext(context);
  vm.runInContext(code, context);
  return context;
}
// The first argument of every fillText call, read with brackets and quotes
// balanced.
function fillTextArgs(source) {
  const out = [];
  let at = 0;
  while ((at = source.indexOf("fillText(", at)) >= 0) {
    let i = at + "fillText(".length;
    const start = i;
    let depth = 0;
    let quote = null;
    for (; i < source.length; i++) {
      const c = source[i];
      if (quote) {
        if (c === "\\") i++;
        else if (c === quote) quote = null;
      } else if (c === "'" || c === '"' || c === "`") quote = c;
      else if ("([{".includes(c)) depth++;
      else if (")]}".includes(c)) { if (depth === 0) break; depth--; }
      else if (c === "," && depth === 0) break;
    }
    out.push(source.slice(start, i).trim());
    at = i;
  }
  return out;
}
const NEGATIVE = [-0.4, -5, -10.5, -950, -999.6, -1000, -1234, -12700];
const noMinus = (text) => assert.doesNotMatch(String(text), /[-−]/, String(text));
const noArrow = (text) => assert.doesNotMatch(String(text), /[↑↓]/, String(text));

describe("power in words", () => {
  it("gives any power as a size", () => {
    for (const w of NEGATIVE) {
      noMinus(words.formatW(w));
      assert.equal(words.formatW(w), words.formatW(-w));
    }
    assert.equal(words.formatW(-1234), "1.2 kW");
    assert.equal(words.formatW(null), "—");
  });

  it("says the direction in a word for both signs, and none near zero", () => {
    const cases = [
      [500, "grid", "500 W import"], [-500, "grid", "500 W export"],
      [1400, "battery", "1.4 kW charge"], [-1400, "battery", "1.4 kW discharge"],
      [3000, "v2x", "3.0 kW charge"], [-3000, "v2x", "3.0 kW discharge"],
      [8, "grid", "8 W"], [-8, "battery", "8 W"], [0, "grid", "0 W"], [-1400, "solar", "1.4 kW"],
    ];
    for (const [w, kind, want] of cases) {
      assert.equal(words.formatPower(w, kind), want, `${w} ${kind}`);
      noMinus(words.formatPower(w, kind));
      noArrow(words.formatPower(w, kind));
    }
  });

  it("keeps Core's sign on Home use until a word is chosen for it", () => {
    assert.equal(words.formatSignedW(-300), "-300 W");
    assert.equal(words.formatSignedW(-1234), "-1.2 kW");
    assert.equal(words.formatSignedW(900), "900 W");
    assert.equal(words.formatSignedW(-0.3), "0 W");
  });

  it("names a target's direction only when it differs from the reading's", () => {
    assert.equal(words.formatTarget(-1200, -1100, "battery"), "→ 1.2 kW");
    assert.equal(words.formatTarget(800, -1100, "battery"), "→ 800 W charge");
    assert.equal(words.formatTarget(-2000, 5, "grid"), "→ 2.0 kW export");
  });

  it("measures how far a device is from its target along the target's direction", () => {
    assert.equal(words.trackingText(-700, -1000), "300 W short");
    assert.equal(words.trackingText(1300, 1000), "300 W over");
    assert.equal(words.trackingText(-1300, -1000), "300 W over");
    assert.equal(words.trackingText(-1005, -1000), "on target");
    // Going the wrong way is short by the whole distance.
    assert.equal(words.trackingText(1300, -1000), "2.3 kW short");
    assert.equal(words.trackingText(1000, -1000), "2.0 kW short");
    assert.equal(words.trackingText(-1000, 1000), "2.0 kW short");
    // A target near zero has no direction; the reading's size is compared.
    assert.equal(words.trackingText(-500, 5), "495 W over");
    assert.equal(words.trackingGap(1300, -1000), -2300);
  });

  it("names both sides of every chart in words, with one separator", () => {
    for (const [chart, sides] of Object.entries(words.SIDES)) {
      for (const text of [sides.above, sides.below]) {
        noArrow(text);
        assert.doesNotMatch(text, /[/,;]|home/, `${chart}: ${text}`);
      }
    }
    assert.equal(words.SIDES.live.below, "export · discharge · solar");
    assert.equal(words.SIDES.live.above, "import · charge · load · car");
    const drawn = [];
    const ctx = {
      save() {}, restore() {}, fillRect() { drawn.push("back"); }, measureText: (t) => ({ width: t.length * 5 }),
      fillText(text, x, y) { drawn.push([text, y]); },
    };
    words.drawSides(ctx, 10, 100, words.SIDES.battery, "ink", "panel");
    assert.deepEqual(drawn, ["back", ["charge", 93], "back", ["discharge", 107]]);
  });
});

describe("the dashboard uses the words where a power goes both ways", () => {
  const app = read("app.js");
  const helpers = ["powerWords", "formatW", "formatPower", "liveStatParts", "updateLiveStat", "batteryTargetLine",
    "tooltipPowerText", "forecastRowText", "batteryRowParts"].map((name) => fn(app, name)).join("\n");
  const sizesOnly = (() => {
    const start = app.indexOf("  var SIZES_ONLY = (function () {");
    assert.ok(start >= 0, "SIZES_ONLY");
    return closed(app, start, app.indexOf("{", start)) + ")();";
  })();

  function dashboard(context = { ftwPowerWords: words }) {
    const els = {};
    // Just enough of an element: a child's text joins its parent's.
    const element = () => ({
      textContent: "", className: "",
      appendChild(child) { this.textContent += child.textContent; },
    });
    const document = { getElementById: (id) => (els[id] ||= element()), createElement: element };
    return { els, ...run(sizesOnly + "\n" + helpers, { ...context, document }) };
  }

  it("writes the live strip with a word for the grid and the battery", () => {
    const d = dashboard();
    d.updateLiveStat("bat", -1400, "is-discharging");
    d.updateLiveStat("grid", -500, "is-export");
    d.updateLiveStat("load", 900, "is-neutral");
    assert.equal(d.els["live-stat-bat"].textContent, "1.4 kW discharge");
    assert.equal(d.els["live-stat-grid"].textContent, "500 W export");
    assert.equal(d.els["live-stat-load"].textContent, "900 W");
    d.updateLiveStat("bat", 1400, "is-charging");
    assert.equal(d.els["live-stat-bat"].textContent, "1.4 kW charge");
    // Home use below zero keeps its sign.
    d.updateLiveStat("load", -300, "is-neutral");
    assert.equal(d.els["live-stat-load"].textContent, "-300 W");
  });

  it("writes the targets, tooltip rows and driver rows with their words", () => {
    const d = dashboard();
    assert.equal(d.batteryTargetLine(-1500), "target 1.5 kW discharge");
    assert.deepEqual({ ...d.tooltipPowerText({ name: "Grid", data: [-500] }, 0) }, { actual: "500 W export", target: "" });
    assert.deepEqual({ ...d.tooltipPowerText({ name: "Sungrow", data: [-1100], target: [-1200] }, 0) },
      { actual: "1.1 kW discharge", target: "→ 1.2 kW" });
    assert.deepEqual({ ...d.tooltipPowerText({ name: "Sungrow", data: [-1100], target: [800] }, 0) },
      { actual: "1.1 kW discharge", target: "→ 800 W charge" });
    assert.deepEqual({ ...d.tooltipPowerText({ name: "Easee EV", data: [3000] }, 0) }, { actual: "3.0 kW", target: "" });
    assert.deepEqual({ ...d.tooltipPowerText({ name: "Load", data: [-300] }, 0) }, { actual: "-300 W", target: "" });
    assert.deepEqual({ ...d.batteryRowParts(-700, -1000) }, { actual: "700 W discharge", target: "→ 1.0 kW", tracking: "300 W short" });
    assert.deepEqual({ ...d.batteryRowParts(1300, -1000) }, { actual: "1.3 kW charge", target: "→ 1.0 kW discharge", tracking: "2.3 kW short" });
    // The warning colour follows the same gap as the words.
    assert.match(app, /var dev = powerWords\(\)\.trackingGap\(batWVal, disp\.target_w\);/);
  });

  it("writes the forecast tooltip's battery and grid with their words", () => {
    const d = dashboard();
    assert.equal(d.forecastRowText({ name: "Battery", val: -1700, showSign: true }), "1.7 kW discharge");
    assert.equal(d.forecastRowText({ name: "Battery", val: 1700, showSign: true }), "1.7 kW charge");
    assert.equal(d.forecastRowText({ name: "Grid", val: -500, showSign: true }), "500 W export");
    assert.equal(d.forecastRowText({ name: "Grid", val: 500, showSign: true }), "500 W import");
    assert.equal(d.forecastRowText({ name: "PV pred", val: 2100 }), "2.1 kW");
    assert.equal(d.forecastRowText({ name: "Load pred", val: -300 }), "-300 W");
    assert.equal(d.forecastRowText({ name: "SoC", val: "64%", literal: true }), "64%");
  });

  it("still draws sizes if power-words.js did not load", () => {
    const d = dashboard({});
    d.updateLiveStat("bat", -1400, "is-discharging");
    assert.equal(d.els["live-stat-bat"].textContent, "1.4 kW");
    assert.equal(d.batteryTargetLine(-1500), "target 1.5 kW");
    assert.equal(d.forecastRowText({ name: "Grid", val: -500, showSign: true }), "500 W");
  });

  it("never prints a signed grid or battery power as a bare size", () => {
    // Each of these can be negative and has no word beside it on screen.
    // (The V2X confirm prints formatW(powerW) after the word "Discharge".)
    // (The forecast rows go through forecastRowText, tested above.)
    const signed = ["batWVal", "meterW", "d.target_w", "disp.target_w", "v2xWVal", "sw", "dev",
      "data.grid_target_w", "Number(gridTargetSlider.value)", "targetW", "t", "d.v2x_dc_w"];
    for (const expr of signed) {
      assert.ok(!app.includes(`formatW(${expr})`), `formatW(${expr}) drops the direction`);
    }
    // Nor as a hand-made "… W", the way the per-phase watts used to print.
    // A size taken with Math.abs first is fine.
    const handMade = [...app.matchAll(/Math\.round\(([^;\n]*?)\)\s*\+\s*" ?W"/g)]
      .map((m) => m[1]).filter((arg) => !/^(abs|Math\.abs\(.*\))$/.test(arg));
    assert.deepEqual(handMade, []);
    // The grid and battery cards print the size and set the word on the next lines.
    for (const [value, word] of [["data.grid_w", "gridDir"], ["data.bat_w", "batDir"]]) {
      const at = app.indexOf(`formatW(${value})`);
      assert.ok(at > 0 && app.slice(at, at + 400).includes(`${word}.textContent`), value);
      assert.equal(app.indexOf(`formatW(${value})`, at + 1), -1, `${value} is printed bare elsewhere`);
    }
    assert.ok(app.includes('bvw.textContent = formatPower(sw, "grid");'), "per-phase watts");
    // The live chart's slot badge uses the flow's arrows: ↓ import, ↑ export.
    assert.ok(app.includes('"15m ↓" + formatWhMagnitude(impWh) + " ↑" + formatWhMagnitude(expWh)'));
  });

  it("words the plan's battery column on the charger card", () => {
    const loadpoints = read("loadpoints.js");
    const { batteryW } = run(fn(loadpoints, "fmtW") + "\n" + fn(loadpoints, "batteryW"), { ftwPowerWords: words });
    assert.equal(batteryW(-1200), "1.2 kW discharge");
    assert.equal(batteryW(900), "900 W charge");
    assert.match(loadpoints, /<td>\$\{batteryW\(a\.battery_w\)\}<\/td>/);
  });
});

describe("charts show sizes, never a raw minus", () => {
  it("labels the plan and diagnose power axes as sizes", () => {
    for (const path of ["plan.js", "diagnose.js"]) {
      const { powerAxisLabel } = run(fn(read(path), "powerAxisLabel"));
      assert.equal(powerAxisLabel(-12700), "12.7kW", path);
      for (const w of NEGATIVE) noMinus(powerAxisLabel(w));
    }
  });

  it("knows every text a chart draws, and none of them starts with a sign", () => {
    // Each fillText on these canvases, by its first argument. A new one
    // fails here until someone says why it cannot start with a sign.
    const known = {
      "app.js": {
        '"waiting for data..."': "words",
        'chartView === "energy" ? yVal.toFixed(1) + " kWh" : formatW(yVal)': "a size; the energy view skips ticks below zero",
        'chartRange + " ago"': "a range",
        '"now"': "words",
        ageStr: "live or an age",
        timeStr: "a clock",
        "lab.name": "a series name",
        'lab.data[i].toFixed(2) + " kWh"': "today's totals, never below zero",
        "text.actual": "tooltipPowerText, tested above",
        "text.target": "tooltipPowerText, tested above",
        'hh + "  predicted"': "a clock",
        "forecastRowText(lab)": "tested above; its literal rows are SoC and price",
        reason: "the plan's reason",
        '"Battery measurements unknown"': "words",
        '(batMax / 1000).toFixed(1) + "kW"': "the largest battery size, never below zero",
        '"100%"': "words", '"0%"': "words", '"24h ago"': "words",
      },
      "plan.js": {
        "'No devices yet. Add one in Settings.'": "words",
        "fmtHHMM(t)": "a clock",
        "toDisplay(priceMax, state.currency).toFixed(0) + ' ' + priceUnit.axis": "the highest price",
        "toDisplay(priceMin, state.currency).toFixed(0)": "the lowest price: a price below zero keeps its sign",
        "'Price'": "words", "'Power'": "words", "'Battery'": "words", "'SoC'": "words",
        "powerAxisLabel(pMagMax)": "a size", "powerAxisLabel(-pMagMax)": "a size",
        "'100%'": "words", "'0%'": "words",
      },
      "diagnose.js": {
        "maxPrice.toFixed(0) + 'ö'": "the highest price, at least 1",
        "'0'": "zero",
        "powerAxisLabel(maxAbsPower)": "a size", "powerAxisLabel(-maxAbsPower)": "a size",
        "'100%'": "words",
        "fmtHHMM(slots[i].slot_start_ms)": "a clock",
      },
      "power-words.js": { text: "the side words, checked above" },
    };
    for (const [path, allowed] of Object.entries(known)) {
      for (const arg of fillTextArgs(read(path))) {
        assert.ok(arg in allowed, `${path}: fillText(${arg}) is new; say why it cannot start with a sign`);
        assert.doesNotMatch(arg, /^(['"`])[-−+]/, `${path}: ${arg}`);
      }
    }
  });

  it("draws the words beside zero on every chart with a lower side", () => {
    const app = read("app.js");
    assert.match(app, /drawSides\(ctx, pad\.left \+ 6, zeroLabelY, powerWords\(\)\.SIDES\.live,/);
    assert.match(app, /drawSides\(ctx, pad\.left \+ 4, batMid, powerWords\(\)\.SIDES\.battery,/);
    const plan = read("plan.js");
    assert.match(plan, /drawSides\(ctx, pad\.l \+ 6, powerYCenter, globalThis\.ftwPowerWords\.SIDES\.plan,/);
    // After the battery bars and the SoC line, so they cannot paint over it.
    assert.ok(plan.indexOf("SIDES.plan") > plan.indexOf("ctx.fillText('SoC'"), "plan side words drawn last");
    assert.match(read("diagnose.js"), /words\.drawSides\(ctx, pad\.l \+ 6, powerMidY, words\.SIDES\.diagnose,/);
    // Loaded before the dashboard script that reads it.
    const html = read("index.html");
    assert.ok(html.indexOf('src="/power-words.js"') < html.indexOf('src="/app.js'));
  });

  it("prints the flow's planets as sizes and keeps the hub's sign", () => {
    const flow = read("components/ftw-energy-flow.js");
    assert.match(flow, /value: p\.placeholder \? "—" : fmtKw\(Math\.abs\(p\.kw\)\),/);
    assert.match(flow, /fmtKw\(load\)/);
    const { fmtKw } = run(fn(flow, "fmtKw"));
    assert.equal(fmtKw(Math.abs(-1.2)), "1.20 kW");
    assert.equal(fmtKw(-0.3), "-300 W");
  });

  it("keeps the direction word under an observed battery and says it is observed", () => {
    const battery = (batW, observeOnly) => flowReadingsFromStatus({
      drivers: { battery: { bat_w: batW, bat_soc: 0.5, observe_only: observeOnly, status: "ok" } },
    }).planets.find((p) => p.role === "battery");
    assert.equal(battery(-1500, true).sub, "discharging");
    assert.equal(battery(1500, true).sub, "charging");
    assert.equal(battery(0, true).sub, "idle");
    assert.equal(battery(-1500, true).observeOnly, true);
    assert.equal(battery(-1500, false).observeOnly, false);
    // "Observe only" and the direction do not fit on one line of the
    // bubble, so the component puts "observe only" under the title.
    const flow = read("components/ftw-energy-flow.js");
    assert.match(flow, /p\.observeOnly \? \(this\._compact \? "OBSERVED" : "OBSERVE ONLY"\) : null/);
    assert.match(flow, /sub: grouped && p\.observeOnly \? "observe only" : p\.sub,/);
  });

  it("says the direction in the plan tooltip and brief", () => {
    const plan = read("plan.js");
    assert.match(plan, /\$\{\(Math\.abs\(a\.battery_w\) \/ 1000\)\.toFixed\(1\)\} kW \(\$\{dir\}\)/);
    assert.match(plan, /<b>\$\{globalThis\.ftwPowerWords\.formatPower\(a\.grid_w, 'grid'\)\}<\/b>/);
    const now = Date.now();
    const brief = derivePlanBrief({
      enabled: true,
      plan: { actions: [{ slot_start_ms: now, slot_len_min: 15, battery_w: -1800, loadpoint_w: 0, pv_limit_w: 0 }] },
      status: { mode: "planner_passive_arbitrage", dispatch: [{ driver: "battery", clamped: true, target_w: -1800 }] },
      now,
    });
    assert.match(brief.constraint, /Safety adjusted battery to 1\.8 kW discharge/);
    noMinus(brief.constraint);
  });
});
