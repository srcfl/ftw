import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const heatingSource = readFileSync(new URL("./heating.js", import.meta.url), "utf8");
const evSource = readFileSync(new URL("./settings/tabs/ev.js", import.meta.url), "utf8");
const readWeb = (name) => readFileSync(new URL(name, import.meta.url), "utf8");

function inertElement() {
  return {
    hidden: false,
    innerHTML: "",
    classList: { add() {}, remove() {}, contains() { return false; } },
    addEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
}

function heatingBody(path) {
  if (path === "/api/drivers") return { pump: { status: "ok" } };
  if (path === "/api/drivers/pump") {
    return { metrics: [{ name: "hp_power_w", value: 800 }] };
  }
  return { points: [] };
}

function rigHeating({ hidden = false } = {}) {
  const documentListeners = new Map();
  const intervals = new Map();
  const fetches = [];
  let nextTimer = 1;
  const section = inertElement();
  const grid = inertElement();
  const document = {
    hidden,
    readyState: "complete",
    head: { appendChild() {} },
    body: inertElement(),
    getElementById(id) {
      if (id === "heating-section") return section;
      if (id === "heating-grid") return grid;
      return null;
    },
    createElement() { return inertElement(); },
    addEventListener(type, fn) { documentListeners.set(type, fn); },
  };
  const sandbox = {
    window: {},
    document,
    fetch(path) {
      const entry = { path: String(path), settled: false };
      entry.promise = new Promise((resolve, reject) => {
        entry.resolve = resolve;
        entry.reject = reject;
      });
      fetches.push(entry);
      return entry.promise;
    },
    setInterval(fn, ms) {
      const id = nextTimer++;
      intervals.set(id, { fn, ms });
      return id;
    },
    clearInterval(id) { intervals.delete(id); },
    console, Date, Math, JSON, Promise, Object, Array, encodeURIComponent,
  };
  sandbox.window.document = document;
  vm.createContext(sandbox);
  vm.runInContext(heatingSource, sandbox);

  async function settleFetches() {
    for (let round = 0; round < 20; round++) {
      const pending = fetches.filter((entry) => !entry.settled);
      if (pending.length === 0) {
        await new Promise((resolve) => setImmediate(resolve));
        if (fetches.every((entry) => entry.settled)) return;
        continue;
      }
      for (const entry of pending) {
        entry.settled = true;
        const body = heatingBody(entry.path);
        entry.resolve({ ok: true, json() { return Promise.resolve(body); } });
      }
      await new Promise((resolve) => setImmediate(resolve));
    }
  }

  return {
    fetches,
    liveFetches() { return fetches.filter((entry) => entry.path === "/api/drivers/pump"); },
    pollTimers() { return [...intervals.values()].filter((timer) => timer.ms === 30_000); },
    setHidden(next) {
      document.hidden = next;
      documentListeners.get("visibilitychange")();
    },
    runPollTimers() {
      for (const timer of [...intervals.values()]) {
        if (timer.ms === 30_000) timer.fn();
      }
    },
    settleFetches,
  };
}

function rigEv({ hidden = false } = {}) {
  const documentListeners = new Map();
  const intervals = new Map();
  const fetches = [];
  const observers = [];
  let nextTimer = 1;
  const modalHidden = new Set();
  let indicator = { className: "", textContent: "" };
  const modal = {
    classList: {
      contains: (name) => modalHidden.has(name),
      add(name) {
        modalHidden.add(name);
        for (const observer of observers) observer.fn();
      },
      remove(name) { modalHidden.delete(name); },
    },
  };
  class MutationObserver {
    constructor(fn) { this.fn = fn; observers.push(this); }
    observe() {}
    disconnect() {}
  }
  const document = {
    hidden,
    getElementById(id) {
      if (id === "ev-status-indicator") return indicator;
      if (id === "settings-modal") return modal;
      return null;
    },
    addEventListener(type, fn) { documentListeners.set(type, fn); },
    removeEventListener(type, fn) {
      if (documentListeners.get(type) === fn) documentListeners.delete(type);
    },
  };
  const windowObj = { FTWSettings: { tabs: {} } };
  const sandbox = {
    window: windowObj,
    document,
    fetch(path) {
      const entry = { path: String(path), settled: false };
      entry.promise = new Promise((resolve, reject) => {
        entry.resolve = resolve;
        entry.reject = reject;
      });
      fetches.push(entry);
      return entry.promise;
    },
    setInterval(fn, ms) {
      const id = nextTimer++;
      intervals.set(id, { fn, ms });
      return id;
    },
    clearInterval(id) { intervals.delete(id); },
    MutationObserver,
    console, JSON, Object, String, Array,
  };
  windowObj.window = windowObj;
  vm.createContext(sandbox);
  vm.runInContext(evSource, sandbox);
  function mount() { sandbox.window.FTWSettings.tabs.ev.after({
    bodyEl: { querySelector() { return null; } },
    config: { ev_charger: {} },
    getByPath() { return ""; },
    captureCurrentTab() {},
    renderTab() {},
  }); }
  mount();
  return {
    indicator() { return indicator; },
    rerender() { indicator = { className: "", textContent: "" }; mount(); },
    fetches,
    pollTimers() { return [...intervals.values()].filter((timer) => timer.ms === 5000); },
    setHidden(next) {
      document.hidden = next;
      documentListeners.get("visibilitychange")();
    },
    closeSettings() { modal.classList.add("hidden"); },
    runPollTimers() {
      for (const timer of [...intervals.values()]) {
        if (timer.ms === 5000) timer.fn();
      }
    },
  };
}

test("heating polling stays single-flight while visible and pauses when hidden", async () => {
  const heating = rigHeating();

  assert.equal(heating.fetches.length, 1, "startup should discover drivers once");
  assert.equal(heating.pollTimers().length, 1, "startup should own one 30s timer");

  heating.runPollTimers();
  heating.runPollTimers();
  assert.equal(heating.fetches.length, 1, "timer ticks must not overlap an unresolved refresh");

  heating.setHidden(true);
  heating.runPollTimers();
  assert.equal(heating.fetches.length, 1, "hidden heating should make no more GETs");
  assert.equal(heating.pollTimers().length, 0, "hidden heating should clear its timer");

  await heating.settleFetches();
  const afterHiddenSettle = heating.liveFetches().length;
  assert.ok(afterHiddenSettle >= 1, "in-flight discovery may finish while hidden");
  assert.equal(heating.fetches.filter((entry) => !entry.settled).length, 0);

  heating.setHidden(false);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(
    heating.liveFetches().length,
    afterHiddenSettle + 1,
    "visible heating should refresh live pump detail once",
  );
  assert.equal(heating.pollTimers().length, 1, "visible heating should restore only one timer");

  heating.runPollTimers();
  heating.runPollTimers();
  assert.equal(
    heating.liveFetches().length,
    afterHiddenSettle + 1,
    "timer ticks must not overlap the visible catch-up",
  );
});

test("heating starts dormant when loaded in a hidden document", () => {
  const heating = rigHeating({ hidden: true });

  assert.equal(heating.fetches.length, 0);
  assert.equal(heating.pollTimers().length, 0);

  heating.setHidden(false);
  assert.equal(heating.fetches.length, 1);
  assert.equal(heating.pollTimers().length, 1);
});

test("EV settings poll pauses when hidden and stops on close", () => {
  const ev = rigEv();

  assert.equal(ev.fetches.length, 1, "opening the EV tab should fetch status once");
  assert.equal(ev.pollTimers().length, 1, "opening the EV tab should own one 5s timer");

  ev.setHidden(true);
  ev.runPollTimers();
  assert.equal(ev.fetches.length, 1, "hidden settings should make no more status GETs");
  assert.equal(ev.pollTimers().length, 0, "hidden settings should clear its timer");

  ev.setHidden(false);
  assert.equal(ev.fetches.length, 2, "visible settings should refresh once");
  assert.equal(ev.pollTimers().length, 1, "visible settings should restore only one timer");

  ev.closeSettings();
  ev.runPollTimers();
  assert.equal(ev.fetches.length, 2, "closing settings should make no more status GETs");
  assert.equal(ev.pollTimers().length, 0, "closing settings should clear its timer");
});

test("plan, cards, and remaining settings pollers hook visibilitychange", () => {
  assert.match(readWeb("./plan.js"), /visibilitychange/);
  assert.match(readWeb("./loadpoints.js"), /visibilitychange/);
  assert.match(readWeb("./twins.js"), /visibilitychange/);
  assert.match(readWeb("./components/ftw-history-card.js"), /visibilitychange/);
  assert.match(readWeb("./components/ftw-savings-card.js"), /visibilitychange/);
  assert.match(readWeb("./components/ftw-price-chart.js"), /visibilitychange/);
  assert.match(readWeb("./settings/tabs/system.js"), /visibilitychange/);
  assert.match(readWeb("./components/ftw-energy-flow.js"), /document\.hidden/);
});


test("EV status polling follows the new element after a provider rerender", async () => {
  const ev = rigEv();
  const old = ev.indicator();
  ev.rerender();
  assert.equal(ev.pollTimers().length, 1);
  for (const entry of ev.fetches) entry.resolve({ json: async () => ({}) });
  await new Promise(resolve => setImmediate(resolve));
  const oldText = old.textContent;
  ev.runPollTimers();
  ev.fetches.at(-1).resolve({ json: async () => ({ drivers: { easee: { status: "online", device_id: "new-status" } } }) });
  await new Promise(resolve => setImmediate(resolve));
  assert.match(ev.indicator().textContent, /new-status/);
  assert.equal(old.textContent, oldText, "the old timer must not update its detached element");
});
