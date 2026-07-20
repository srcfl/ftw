import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./models.js", import.meta.url), "utf8");

function element() {
  const listeners = new Map();
  return {
    className: "",
    classList: { add() {}, contains() { return false; }, remove() {} },
    innerHTML: "",
    style: {},
    textContent: "",
    addEventListener(name, handler) { listeners.set(name, handler); },
    click() { listeners.get("click")?.({ target: this }); },
    querySelectorAll() { return []; },
  };
}

async function loadSelfTune({ observeOnly = false } = {}) {
  const elements = new Map([
    ["models-grid", element()],
    ["self-tune-btn", element()],
    ["self-tune-modal", element()],
    ["self-tune-close", element()],
    ["self-tune-start", element()],
    ["self-tune-cancel", element()],
    ["self-tune-body", element()],
    ["self-tune-status", element()],
  ]);
  const requests = [];
  const document = {
    body: element(),
    addEventListener() {},
    createElement() { return element(); },
    getElementById(id) { return elements.get(id) || null; },
  };
  const fetch = async (path, options = {}) => {
    requests.push({ path, options });
    const payload = path === "/api/battery_models"
      ? { bat_a: {} }
      : path === "/api/self_tune/status"
        ? {
            active: false,
            before: { bat_a: { tau_s: 1, gain: 1 } },
            after: { bat_a: { tau_s: 2, gain: 2 } },
          }
        : path === "/api/status"
          ? { drivers: { bat_a: { bat_w: 0, observe_only: observeOnly } } }
          : {};
    return { ok: true, json: async () => payload };
  };

  vm.runInNewContext(source, {
    clearInterval() {},
    document,
    fetch,
    setInterval() { return 1; },
    window: {},
  });
  elements.get("self-tune-btn").click();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  return { elements, requests };
}

test("Run again reuses the completed battery selection", async () => {
  const { elements, requests } = await loadSelfTune();
  elements.get("self-tune-start").click();
  await new Promise((resolve) => setImmediate(resolve));

  const start = requests.find((request) => request.path === "/api/self_tune/start");
  assert.ok(start);
  assert.deepEqual(JSON.parse(start.options.body), { batteries: ["bat_a"] });
});

test("Run again does not select an observe-only battery", async () => {
  const { elements, requests } = await loadSelfTune({ observeOnly: true });
  elements.get("self-tune-start").click();
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(requests.some((request) => request.path === "/api/self_tune/start"), false);
  assert.equal(elements.get("self-tune-status").textContent, "Select at least one battery");
});
