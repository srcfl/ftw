import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const statusPaths = new Set(["/api/status", "/api/loadpoints", "/api/cars", "/api/health"]);
const liveHistoryPath = "/api/history?range=24h&points=288";
const isHistoryPath = (path) => path.startsWith("/api/history?");
const apiReadTimeoutMs = 15_000;

function inertElement() {
  const classList = {
    add() {}, remove() {}, toggle() { return false; }, contains() { return false; },
  };
  const context = new Proxy({}, {
    get(target, key) {
      if (!(key in target)) target[key] = function () {};
      return target[key];
    },
    set(target, key, value) { target[key] = value; return true; },
  });
  return {
    classList, dataset: {}, style: {}, children: [], childNodes: [],
    parentNode: { insertBefore() {} },
    textContent: "", innerHTML: "", value: "", checked: false, hidden: false,
    addEventListener() {}, removeEventListener() {}, appendChild() {}, append() {},
    replaceChildren() {}, remove() {}, focus() {}, click() {},
    setAttribute() {}, removeAttribute() {}, getAttribute() { return null; },
    querySelector() { return null; }, querySelectorAll() { return []; },
    closest() { return null; }, matches() { return false; },
    getContext() { return context; },
    getBoundingClientRect() { return { width: 800, height: 400, left: 0, top: 0 }; },
  };
}

function rig({ hidden = false } = {}) {
  const documentListeners = new Map();
  const windowListeners = new Map();
  const intervals = new Map();
  const timeouts = new Map();
  const fetches = [];
  let nextTimer = 1;

  class TestAbortSignal {
    constructor() {
      this.aborted = false;
      this.listeners = new Set();
    }
    addEventListener(type, listener) {
      if (type === "abort") this.listeners.add(listener);
    }
    abort() {
      if (this.aborted) return;
      this.aborted = true;
      for (const listener of this.listeners) listener();
      this.listeners.clear();
    }
  }

  class TestAbortController {
    constructor() { this.signal = new TestAbortSignal(); }
    abort() { this.signal.abort(); }
  }

  const document = {
    hidden,
    body: inertElement(),
    documentElement: inertElement(),
    getElementById() { return inertElement(); },
    querySelector() { return inertElement(); },
    querySelectorAll() { return []; },
    createElement() { return inertElement(); },
    createTextNode(text) { return { textContent: text }; },
    createDocumentFragment() { return inertElement(); },
    addEventListener(type, fn) { documentListeners.set(type, fn); },
  };
  const window = {
    innerWidth: 1024, innerHeight: 768, devicePixelRatio: 1,
    addEventListener(type, fn) { windowListeners.set(type, fn); },
    dispatchEvent() {},
    customElements: { whenDefined() { return new Promise(() => {}); } },
  };
  const storage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  const sandbox = {
    window, document, localStorage: storage, sessionStorage: storage,
    customElements: window.customElements,
    AbortController: TestAbortController,
    fetch(path, opts = {}) {
      const entry = { path: String(path), settled: false, aborted: false };
      entry.promise = new Promise((resolve, reject) => {
        entry.resolve = resolve;
        entry.reject = reject;
      });
      const abort = () => {
        if (entry.settled) return;
        entry.settled = true;
        entry.aborted = true;
        const err = new Error("request aborted");
        err.name = "AbortError";
        entry.reject(err);
      };
      if (opts.signal) {
        if (opts.signal.aborted) abort();
        else opts.signal.addEventListener("abort", abort, { once: true });
      }
      fetches.push(entry);
      return entry.promise;
    },
    setInterval(fn, ms) {
      const id = nextTimer++;
      intervals.set(id, { fn, ms });
      return id;
    },
    clearInterval(id) { intervals.delete(id); },
    setTimeout(fn, ms) {
      const id = nextTimer++;
      timeouts.set(id, { fn, ms });
      return id;
    },
    clearTimeout(id) { timeouts.delete(id); },
    requestAnimationFrame() { return 0; }, cancelAnimationFrame() {},
    getComputedStyle() { return { getPropertyValue() { return ""; } }; },
    ResizeObserver: class { observe() {} disconnect() {} },
    MutationObserver: class { observe() {} disconnect() {} },
    console, Date, Math, JSON, Promise, Map, Set, URL, URLSearchParams,
  };
  window.window = window;
  window.document = document;
  window.localStorage = storage;
  sandbox.globalThis = sandbox;

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  function responseFor(path) {
    let body = {};
    if (path === "/api/loadpoints") body = { loadpoints: [] };
    if (path === "/api/health") body = { storage: null };
    if (isHistoryPath(path)) body = { items: [] };
    return { ok: true, status: 200, json() { return Promise.resolve(body); } };
  }

  async function settleFetches(matcher) {
    const pending = fetches.filter((entry) => matcher(entry.path) && !entry.settled);
    for (const entry of pending) {
      entry.settled = true;
      entry.resolve(responseFor(entry.path));
    }
    await new Promise((resolve) => setImmediate(resolve));
  }

  return {
    document,
    statusFetches() { return fetches.filter((entry) => statusPaths.has(entry.path)); },
    historyFetches() { return fetches.filter((entry) => isHistoryPath(entry.path)); },
    liveHistoryFetches() { return fetches.filter((entry) => entry.path === liveHistoryPath); },
    statusTimers() { return [...intervals.values()].filter((timer) => timer.ms === 2000); },
    liveHistoryTimers() { return [...intervals.values()].filter((timer) => timer.ms === 60_000); },
    setHidden(hidden) {
      document.hidden = hidden;
      documentListeners.get("visibilitychange")();
    },
    runStatusTimers() {
      for (const timer of [...intervals.values()]) {
        if (timer.ms === 2000) timer.fn();
      }
    },
    runLiveHistoryTimers() {
      for (const timer of [...intervals.values()]) {
        if (timer.ms === 60_000) timer.fn();
      }
    },
    async expireApiReads() {
      const due = [...timeouts.entries()].filter(([, timer]) => timer.ms === apiReadTimeoutMs);
      for (const [id, timer] of due) {
        timeouts.delete(id);
        timer.fn();
      }
      await new Promise((resolve) => setImmediate(resolve));
    },
    settleStatusFetches() { return settleFetches((path) => statusPaths.has(path)); },
    settleHistoryFetches() { return settleFetches(isHistoryPath); },
    settleLiveHistoryFetches() { return settleFetches((path) => path === liveHistoryPath); },
  };
}

test("dashboard polling stays single-flight while visible", async () => {
  const app = rig();

  assert.equal(app.statusFetches().length, 4, "startup should make the four status GETs once");
  assert.equal(app.historyFetches().length, 2, "startup should load each distinct history view once");
  assert.equal(app.liveHistoryFetches().length, 1, "startup should fetch live history once");
  assert.equal(app.statusTimers().length, 1, "startup should own one status timer");
  assert.equal(app.liveHistoryTimers().length, 1, "startup should own one live-history timer");

  app.runStatusTimers();
  app.runStatusTimers();
  app.runLiveHistoryTimers();
  app.runLiveHistoryTimers();
  assert.equal(app.statusFetches().length, 4, "timer ticks must not overlap an unresolved status poll");
  assert.equal(app.historyFetches().length, 2, "timer ticks must not duplicate either unresolved history request");
  assert.equal(app.liveHistoryFetches().length, 1, "forced timer ticks must reuse unresolved live history");

  await app.settleStatusFetches();
  app.runStatusTimers();
  assert.equal(app.statusFetches().length, 8, "a future tick should poll after the prior request settles");
  app.runStatusTimers();
  assert.equal(app.statusFetches().length, 8, "the replacement poll should also stay single-flight");

  app.setHidden(true);
  app.runStatusTimers();
  app.runLiveHistoryTimers();
  assert.equal(app.statusFetches().length, 8, "hidden dashboard should make no more status GETs");
  assert.equal(app.liveHistoryFetches().length, 1, "hidden dashboard should make no more live-history GETs");
  assert.equal(app.statusTimers().length, 0, "hidden dashboard should clear its status timer");
  assert.equal(app.liveHistoryTimers().length, 0, "hidden dashboard should clear its live-history timer");

  await app.settleStatusFetches();
  await app.settleLiveHistoryFetches();
  assert.equal(app.liveHistoryFetches().length, 1, "settling while hidden must not start live-history catch-up");
  assert.equal(app.statusFetches().length, 8, "settling while hidden must not start a catch-up poll");

  app.setHidden(false);
  assert.equal(app.statusFetches().length, 12, "visible dashboard should refresh all four endpoints at once");
  assert.equal(app.historyFetches().length, 3, "visible catch-up should reuse the unresolved chart load");
  assert.equal(app.liveHistoryFetches().length, 2, "visible dashboard should refresh live history at once");
  assert.equal(app.statusTimers().length, 1, "visible dashboard should restore only one status timer");
  assert.equal(app.liveHistoryTimers().length, 1, "visible dashboard should restore only one live-history timer");

  app.runStatusTimers();
  app.runStatusTimers();
  app.runLiveHistoryTimers();
  app.runLiveHistoryTimers();
  assert.equal(app.statusFetches().length, 12, "timer ticks must not overlap the visible catch-up poll");
  assert.equal(app.liveHistoryFetches().length, 2, "timer and visibility refreshes must share one live-history request");

  await app.settleLiveHistoryFetches();
  app.runLiveHistoryTimers();
  assert.equal(app.liveHistoryFetches().length, 3, "a future timer should refresh after live history settles");
});

test("dashboard starts dormant when loaded in a hidden document", () => {
  assert.match(source, /let animating = !document\.hidden/);
  const app = rig({ hidden: true });

  assert.equal(app.statusFetches().length, 0);
  assert.equal(app.historyFetches().length, 0, "hidden startup must make no history request");
  assert.equal(app.liveHistoryFetches().length, 0);
  assert.equal(app.statusTimers().length, 0);
  assert.equal(app.liveHistoryTimers().length, 0);

  app.setHidden(false);
  assert.equal(app.statusFetches().length, 4);
  assert.equal(app.historyFetches().length, 2, "first visible event should load both history views at once");
  assert.equal(app.liveHistoryFetches().length, 1);
  assert.equal(app.statusTimers().length, 1);
  assert.equal(app.liveHistoryTimers().length, 1);

  app.setHidden(true);
  app.setHidden(false);
  assert.equal(app.historyFetches().length, 2, "hide/show must reuse both unresolved history requests");
  assert.equal(app.statusTimers().length, 1);
  assert.equal(app.liveHistoryTimers().length, 1);
});

test("a timed-out read releases both single-flight gates", async () => {
  const app = rig();

  assert.equal(app.statusFetches().length, 4);
  assert.equal(app.liveHistoryFetches().length, 1);

  // None of the startup reads settle. Expire their controlled deadlines in one
  // step, then let promise rejection and finally handlers drain.
  await app.expireApiReads();
  assert.equal(app.statusFetches().filter((entry) => entry.aborted).length, 4);
  assert.equal(app.historyFetches().filter((entry) => entry.aborted).length, 2);

  app.runStatusTimers();
  app.runLiveHistoryTimers();
  assert.equal(app.statusFetches().length, 8, "status should retry after its timed-out poll");
  assert.equal(app.liveHistoryFetches().length, 2, "forced history should retry after its timed-out read");

  // The replacement reads are still unresolved, so normal single-flight still
  // coalesces later timer ticks.
  app.runStatusTimers();
  app.runLiveHistoryTimers();
  assert.equal(app.statusFetches().length, 8);
  assert.equal(app.liveHistoryFetches().length, 2);
});
