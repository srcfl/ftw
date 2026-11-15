import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const css = readFileSync(new URL("./app.css", import.meta.url), "utf8");
const card = readFileSync(new URL("./components/ftw-history-card.js", import.meta.url), "utf8");

function makeHistoryCard(apiFetch) {
  const source = card
    .replace(/^import .*;\r?\n/gm, "")
    .replace('customElements.define("ftw-history-card", FtwHistoryCard);',
      "globalThis.FtwHistoryCard = FtwHistoryCard;");
  class FtwElement {
    constructor() {
      this.attributes = new Map();
      this.shadowRoot = { querySelector() { return null; } };
      this.style = { setProperty() {} };
    }
    getAttribute(name) { return this.attributes.get(name) ?? null; }
    hasAttribute(name) { return this.attributes.has(name); }
    setAttribute(name, value) { this.attributes.set(name, String(value)); }
    removeAttribute(name) { this.attributes.delete(name); }
  }
  const context = vm.createContext({
    FtwElement,
    apiFetch,
    ftwDebugDelay: () => 0,
    customElements: { define() {} },
  });
  vm.runInContext(source, context, { filename: "ftw-history-card.js" });
  return new context.FtwHistoryCard();
}

async function refreshCard(card, range, apiFetch) {
  card._range = range;
  card.setAttribute("metric", "import");
  card._chart = {
    style: { minWidth: "" },
    attributes: new Map(),
    setAttribute(name, value) { this.attributes.set(name, value); },
    removeAttribute(name) { this.attributes.delete(name); },
  };
  card._totalEl = { textContent: "", innerHTML: "" };
  card._refresh();
  await new Promise((resolve) => setImmediate(resolve));
  return card._chart;
}

test("history offers week, this month, last month and an accessible month picker", () => {
  assert.match(html, /data-range="week"[^>]*>Week/);
  assert.match(html, /data-range="this-month"[^>]*>This month/);
  assert.match(html, /data-range="last-month"[^>]*>Last month/);
  assert.match(html, /type="month" aria-label="Choose history month"/);
});

test("shared bars and pie request the same selected calendar month", () => {
  assert.match(app, /month=" \+ encodeURIComponent\(historyState\.range\.slice\(6\)\)/);
  assert.match(app, /function setHistoryRange\(range, active\) \{[\s\S]*?historyState\.range = range;[\s\S]*?tile\.setAttribute\("range", range\)/);
  assert.match(card, /month=" \+ encodeURIComponent\(month\)/);
  assert.match(card, /function currentMonth\(\)/);
});

test("a redundant range notification does not reset a loaded history card", () => {
  const callback = card.match(/attributeChangedCallback\(name\)\s*\{([\s\S]*?)\n  \}/)?.[1];
  assert.ok(callback, "history-card attribute callback exists");

  // FtwElement.update() replaces the card shadow DOM. Same-range attribute
  // writes must therefore return before update(), or they recreate a loading
  // chart without a corresponding refresh.
  assert.match(callback, /if \(!next \|\| next === this\._range\) return;/);
  assert.match(callback, /this\.update\(\);[\s\S]*if \(rangeChanged\) this\._refresh\(\);/);
  assert.match(card, /if \(seq !== this\._reqSeq\) return;/);

  const onAttributeChanged = new Function("name", callback);
  const cards = Array.from({ length: 4 }, () => {
    const el = {
      range: "month:2026-10",
      _range: "month:2026-10",
      updates: 0,
      refreshes: 0,
      getAttribute(name) { return name === "range" ? this.range : null; },
      update() { this.updates++; },
      _refresh() { this.refreshes++; },
    };
    return el;
  });

  for (const range of ["month:2026-09", "month:2025-03", "month:2026-10"]) {
    for (const el of cards) {
      el.range = range;
      onAttributeChanged.call(el, "range");
      assert.equal(el.updates, el.refreshes, "a real selection updates and refreshes");
      assert.equal(el._range, range);
      // The app may redundantly write its authoritative range. This must not
      // recreate the shadow DOM after the response has rendered the chart.
      onAttributeChanged.call(el, "range");
      assert.equal(el.updates, el.refreshes);
    }
  }
});

test("shared month controls target all four history tiles", () => {
  assert.match(html, /id="history-tiles"/);
  assert.equal((html.match(/<ftw-history-card metric=/g) || []).length, 4);
  assert.match(app, /querySelectorAll\("\.history-tiles ftw-history-card"\)/);
  assert.match(app, /historyState\.range/);
});

test("picked-month selection hides the misleading month-button pill", () => {
  const pickerActiveExpression = app.match(
    /var active = (selectedHistoryMonth === currentMonth \? "this-month" : "picked-month");/,
  )?.[1];
  assert.ok(pickerActiveExpression, "picker distinguishes current from historical months");
  const pickerActive = new Function("selectedHistoryMonth", "currentMonth", `return ${pickerActiveExpression};`);
  const activeBody = app.match(/function setHistoryPeriodActive\(active\) \{([\s\S]*?)\n  \}/)?.[1];
  assert.ok(activeBody, "period controls share one active-state updater");
  const setActive = new Function("active", "historyToggle", "historyTimeZone", "historyMonthPicker", activeBody);
  const buttons = ["week", "this-month", "last-month"].map((action) => ({
    action,
    pressed: null,
    active: false,
    disabled: false,
    getAttribute(name) { return name === "data-range" ? this.action : null; },
    setAttribute(name, value) { if (name === "aria-pressed") this.pressed = value; },
    classList: { toggle(name, value) { if (name === "active") this.owner.active = value; } },
  }));
  buttons.forEach((button) => { button.classList.owner = button; });
  const historyToggle = {
    active: null,
    setAttribute(name, value) { if (name === "data-active") this.active = value; },
    querySelectorAll() { return buttons; },
  };
  const historyMonthPicker = { disabled: true };
  const update = (month, currentMonth) => {
    setActive(pickerActive(month, currentMonth), historyToggle, "Europe/London", historyMonthPicker);
  };

  update("2026-10", "2026-10");
  assert.equal(historyToggle.active, "this-month");
  assert.deepEqual(buttons.map((button) => button.pressed), ["false", "true", "false"]);
  update("2026-09", "2026-10");
  assert.equal(historyToggle.active, "picked-month");
  assert.deepEqual(buttons.map((button) => button.pressed), ["false", "false", "false"]);
  assert.deepEqual(buttons.map((button) => button.active), [false, false, false]);

  const pickedPill = css.match(/#history-toggle\[data-active="picked-month"\]::before\s*\{([^}]*)\}/)?.[1];
  assert.ok(pickedPill, "picked month has an explicit pill rule");
  assert.match(pickedPill, /opacity:\s*0\s*;/);
  assert.doesNotMatch(pickedPill, /transform:/);

  update("2026-10", "2026-10");
  assert.match(css, /#history-toggle\[data-active="this-month"\]::before\s*\{\s*transform:\s*translateX\(100%\)/);
  update("2026-09", "2026-09");
  assert.equal(historyToggle.active, "this-month", "choosing the runtime current month restores its pill");
  assert.deepEqual(buttons.map((button) => button.pressed), ["false", "true", "false"]);
  setActive("last-month", historyToggle, "Europe/London", historyMonthPicker);
  assert.equal(historyToggle.active, "last-month");
  assert.deepEqual(buttons.map((button) => button.pressed), ["false", "false", "true"]);
  assert.match(css, /#history-toggle\[data-active="last-month"\]::before\s*\{\s*transform:\s*translateX\(200%\)/);
});

test("month charts scroll at a readable width and return to the compact week width", async () => {
  const calls = [];
  const apiFetch = async (url) => {
    calls.push(url);
    const count = url.includes("month=") ? 31 : 7;
    return {
      ok: true,
      json: async () => ({ days: Array.from({ length: count }, (_, i) => ({
        day: `2026-10-${String(i + 1).padStart(2, "0")}`,
        import_wh: 1200,
      })) }),
    };
  };
  const historyCard = makeHistoryCard(apiFetch);
  const markup = historyCard.render();
  assert.match(markup, /<div class="chart-scroll" tabindex="0" role="region" aria-label="Daily energy chart" aria-describedby="scroll-hint">/);
  assert.match(card, /\.chart-scroll \{ overflow-x: auto; min-width: 0; \}/);
  assert.match(card, /\.chart-scroll ftw-bar-chart \{ display: block; \}/);

  const chart = await refreshCard(historyCard, "month:2026-10", apiFetch);
  assert.equal(calls.at(-1), "/api/energy/daily?month=2026-10");
  assert.equal(chart.data.length, 31);
  assert.equal(chart.style.minWidth, "1426px");

  await refreshCard(historyCard, "week", apiFetch);
  assert.equal(calls.at(-1), "/api/energy/daily?days=7");
  assert.equal(historyCard._chart.data.length, 7);
  assert.equal(historyCard._chart.style.minWidth, "", "a shorter range clears the monthly width");
});


test("chart scroll hint appears only for overflow and points to available days", () => {
  const el = makeHistoryCard(async () => {});
  const region = { clientWidth: 300, scrollWidth: 1400, scrollLeft: 0 };
  const hint = {};
  el.shadowRoot = { querySelector(selector) { return selector === ".chart-scroll" ? region : hint; } };
  el._syncScrollHint();
  assert.equal(hint.hidden, false);
  assert.equal(hint.textContent, "Scroll for more days →");
  region.scrollLeft = 500;
  el._syncScrollHint();
  assert.equal(hint.textContent, "← Scroll for more days →");
  region.scrollLeft = 1100;
  el._syncScrollHint();
  assert.equal(hint.textContent, "← Scroll for earlier days");
  region.scrollWidth = 300;
  region.scrollLeft = 0;
  el._syncScrollHint();
  assert.equal(hint.hidden, true);
});
