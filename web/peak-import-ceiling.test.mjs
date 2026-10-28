import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

// #1674: switching the peak import ceiling on applied 3,000 W that the
// browser kept from a test, not the 7,500 W the owner meant. These run the
// real app.js against a fake DOM: switching on must apply the box's last
// ceiling and nothing this browser stored.

const source = readFileSync(new URL("./app.js", import.meta.url), "utf8");
// index.html loads the shared power words before app.js.
const powerWords = readFileSync(new URL("./power-words.js", import.meta.url), "utf8");

function element(id) {
  const listeners = new Map();
  const context = new Proxy({}, {
    get(target, key) {
      if (!(key in target)) target[key] = function () {};
      return target[key];
    },
    set(target, key, value) { target[key] = value; return true; },
  });
  return {
    id, listeners,
    classList: { add() {}, remove() {}, toggle() { return false; }, contains() { return false; } },
    dataset: {}, style: {}, children: [], childNodes: [],
    parentNode: { insertBefore() {} },
    textContent: "", innerHTML: "", value: "", defaultValue: "", checked: false, disabled: false, hidden: false,
    addEventListener(type, fn) { listeners.set(type, fn); },
    removeEventListener() {}, appendChild() {}, append() {},
    replaceChildren() {}, remove() {}, focus() {}, click() {},
    setAttribute() {}, removeAttribute() {}, getAttribute() { return null; },
    querySelector() { return null; }, querySelectorAll() { return []; },
    closest() { return null; }, matches() { return false; },
    getContext() { return context; },
    getBoundingClientRect() { return { width: 800, height: 400, left: 0, top: 0 }; },
  };
}

function rig(status) {
  const elements = new Map();
  const byId = (id) => {
    if (!elements.has(id)) elements.set(id, element(id));
    return elements.get(id);
  };
  // As index.html ships the control: off, slider disabled at 5,000 W.
  Object.assign(byId("peak-limit-slider"), { value: "5000", defaultValue: "5000", disabled: true });
  Object.assign(byId("peak-limit-send"), { disabled: true });

  const posts = [];
  const document = {
    hidden: false,
    body: element("body"),
    documentElement: element("html"),
    getElementById: byId,
    querySelector() { return element(""); },
    querySelectorAll() { return []; },
    createElement() { return element(""); },
    createTextNode(text) { return { textContent: text }; },
    createDocumentFragment() { return element(""); },
    addEventListener() {},
  };
  const window = {
    innerWidth: 1024, innerHeight: 768, devicePixelRatio: 1,
    addEventListener() {}, dispatchEvent() {},
    customElements: { whenDefined() { return new Promise(() => {}); } },
  };
  // Göran's browser: a 3,000 W test value left in storage.
  const kept = new Map([["ftw-peak-import-ceiling-w", "3000"]]);
  const storage = {
    getItem(key) { return kept.has(key) ? kept.get(key) : null; },
    setItem(key, value) { kept.set(key, String(value)); },
    removeItem(key) { kept.delete(key); },
  };
  const json = (body) => Promise.resolve({ ok: true, status: 200, json() { return Promise.resolve(body); } });
  const sandbox = {
    window, document, localStorage: storage, sessionStorage: storage,
    customElements: window.customElements,
    AbortController: class { constructor() { this.signal = { aborted: false, addEventListener() {} }; } abort() {} },
    fetch(path, opts = {}) {
      path = String(path);
      if (opts.method === "POST") {
        posts.push({ path, body: JSON.parse(opts.body) });
        return json({ status: "ok" });
      }
      if (path === "/api/status") return json(status);
      if (path === "/api/loadpoints") return json({ loadpoints: [] });
      if (path.startsWith("/api/history?")) return json({ items: [] });
      return json({});
    },
    setInterval() { return 1; }, clearInterval() {},
    setTimeout() { return 1; }, clearTimeout() {},
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
  vm.runInContext(powerWords, sandbox);
  vm.runInContext(source, sandbox);

  const settle = () => new Promise((resolve) => setImmediate(resolve));
  return {
    posts, kept,
    el: byId,
    settle,
    async toggle(on) {
      const t = byId("peak-limit-enabled-toggle");
      t.checked = on;
      t.listeners.get("change")();
      await settle();
    },
  };
}

const ceilingPosts = (app) => app.posts.filter((p) => p.path === "/api/peak_import_ceiling");

test("switching on applies the box's last ceiling, not the browser's", async () => {
  const app = rig({ peak_import_ceiling_w: 0, peak_import_ceiling_last_w: 7500 });
  await app.settle();
  assert.equal(Number(app.el("peak-limit-slider").value), 7500, "off, the slider shows what switching on applies");
  assert.equal(app.el("peak-limit-value").textContent, "7.5 kW");

  await app.toggle(true);
  assert.deepEqual(ceilingPosts(app).map((p) => p.body), [{ peak_import_ceiling_w: 7500 }]);
  assert.equal(app.el("peak-limit-value").textContent, "7.5 kW");
  assert.equal(app.kept.get("ftw-peak-import-ceiling-w"), "3000", "the page neither reads nor writes its old copy");
});

test("with no ceiling ever on, switching on waits for Save", async () => {
  const app = rig({ peak_import_ceiling_w: 0, peak_import_ceiling_last_w: null });
  await app.settle();
  assert.equal(Number(app.el("peak-limit-slider").value), 5000);

  await app.toggle(true);
  assert.equal(ceilingPosts(app).length, 0, "nothing applies before the owner picks a value");
  assert.equal(app.el("peak-limit-slider").disabled, false);
  assert.equal(app.el("peak-limit-send").disabled, false);

  app.el("peak-limit-slider").value = "7500";
  app.el("peak-limit-slider").listeners.get("input")();
  app.el("peak-limit-send").listeners.get("click")();
  await app.settle();
  assert.deepEqual(ceilingPosts(app).map((p) => p.body), [{ peak_import_ceiling_w: 7500 }]);
});

test("switching off applies 0", async () => {
  const app = rig({ peak_import_ceiling_w: 7500, peak_import_ceiling_last_w: 7500 });
  await app.settle();
  assert.equal(app.el("peak-limit-enabled-toggle").checked, true);

  await app.toggle(false);
  assert.deepEqual(ceilingPosts(app).map((p) => p.body), [{ peak_import_ceiling_w: 0 }]);
});
