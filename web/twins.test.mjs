import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./twins.js", import.meta.url), "utf8");
const now = Date.UTC(2026, 8, 7, 10, 30, 0);

function model(status = "learning") {
  return {
    enabled: true,
    samples: 41,
    mae_w: 182,
    quality: 0.6,
    last_ms: now - 60_000,
    learning: {
      engine: "energyplan",
      status,
      started_ms: now - 3_600_000,
      latest_training_ms: now - 60_000,
      reset_available: status !== "unavailable",
    },
  };
}

async function settle() {
  for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
}

function load({ pv = model(), loadModel = model("ready"), confirm = () => true, post } = {}) {
  const listeners = new Map();
  let html = "";
  let focused = null;
  let document;
  const grid = {
    addEventListener(type, handler) { listeners.set(type, handler); },
    querySelector(selector) {
      const match = selector.match(/data-(reset-twin|loadmodel-profile)="([^"]+)"/);
      if (!match || !html.includes(`data-${match[1]}="${match[2]}"`)) return null;
      const tag = html.match(new RegExp(`<button[^>]*data-${match[1]}="${match[2]}"[^>]*>`));
      return {
        disabled: /\sdisabled(?:\s|=|>)/.test(tag?.[0] || ""),
        focus() {
          document.activeElement = { dataset: { [match[1]]: match[2] } };
          focused = match[1] === "reset-twin" ? match[2] : "profile:" + match[2];
        },
      };
    },
  };
  Object.defineProperty(grid, "innerHTML", {
    get() { return html; },
    set(value) {
      if (document?.activeElement?.dataset?.resetTwin || document?.activeElement?.dataset?.loadmodelProfile) document.activeElement = document.body;
      html = String(value);
    },
  });
  const subtitle = { textContent: "" };
  const requests = [];
  document = {
    readyState: "complete",
    body: { classList: { contains: value => value === "advanced" } },
    activeElement: null,
    addEventListener() {},
    getElementById(id) {
      return id === "twins-grid" ? grid : id === "twins-subtitle" ? subtitle : null;
    },
  };
  const response = (body, ok = true, status = ok ? 200 : 500) => ({ ok, status, json: async () => body });
  const fetch = (path, options = {}) => {
    requests.push({ path, options });
    if (options.method === "POST") return post ? post(path, options) : Promise.resolve(response({}));
    if (path === "/api/pvmodel") return Promise.resolve(response(pv));
    if (path === "/api/loadmodel") return Promise.resolve(response(loadModel));
    return Promise.resolve(response({}));
  };
  vm.runInNewContext(source, {
    document, fetch, confirm, setInterval: () => 1, clearInterval() {}, Date: class extends Date {
      static now() { return now; }
    }, Number, Math, String, Map, Set, Promise, Error,
  }, { filename: "twins.js" });
  return {
    grid, subtitle, requests,
    click(endpoint) {
      listeners.get("click")({ target: { dataset: { resetTwin: endpoint } } });
    },
    setFocused(endpoint) { document.activeElement = { dataset: { resetTwin: endpoint } }; },
    clickProfile(profile) {
      listeners.get("click")({ target: { dataset: { loadmodelProfile: profile }, classList: { contains: () => false } } });
    },
    setFocusedProfile(profile) { document.activeElement = { dataset: { loadmodelProfile: profile } }; focused = "profile:" + profile; },
    focused: () => focused,
  };
}

test("renders the Energyplan learning state, local training times, and legacy stats as secondary", async () => {
  const ui = load();
  await settle();

  assert.match(ui.grid.innerHTML, /<h3>Solar production<\/h3>/);
  assert.match(ui.grid.innerHTML, /<span>engine<\/span><b>Energyplan<\/b>/);
  assert.match(ui.grid.innerHTML, /<span>learning state<\/span><b>Learning<\/b>/);
  assert.match(ui.grid.innerHTML, /<span>learning started<\/span><b>.*2026.*<\/b>/);
  assert.match(ui.grid.innerHTML, /legacy model stats/);
  assert.match(ui.grid.innerHTML, /legacy samples/);
  assert.match(ui.grid.innerHTML, /role="status" aria-live="polite"/);
  assert.doesNotMatch(ui.grid.innerHTML, /50 minutes/i);
  assert.equal(ui.subtitle.textContent, "Forecast learning for solar production and consumption");
});

test("confirmation cancellation sends no reset and names the protected history and other model", async () => {
  const questions = [];
  const ui = load({ confirm: question => { questions.push(question); return false; } });
  await settle();
  ui.click("/api/pvmodel/reset");
  await settle();

  assert.equal(ui.requests.filter(request => request.options.method === "POST").length, 0);
  assert.match(questions[0], /solar production/);
  assert.match(questions[0], /measured history/i);
  assert.match(questions[0], /consumption model/i);
  assert.match(questions[0], /lower/i);
});

test("a pending relearn disables only its action, keeps focus through a repaint, and prevents a second POST", async () => {
  let resolvePost;
  const post = () => new Promise(resolve => { resolvePost = resolve; });
  const ui = load({ post });
  await settle();
  ui.setFocused("/api/pvmodel/reset");
  ui.click("/api/pvmodel/reset");
  ui.click("/api/pvmodel/reset");

  assert.match(ui.grid.innerHTML, /Starting new learning period…/);
  assert.match(ui.grid.innerHTML, /data-reset-twin="\/api\/pvmodel\/reset"[^>]*disabled/);
  assert.equal(ui.focused(), null, "the pending disabled button cannot keep focus");
  assert.equal(ui.requests.filter(request => request.options.method === "POST").length, 1);

  resolvePost({ ok: true, status: 200, json: async () => ({}) });
  await settle();
  assert.match(ui.grid.innerHTML, /The box accepted the request/);
  assert.equal(ui.focused(), "/api/pvmodel/reset", "completion returns keyboard focus to the enabled action");
});

test("completion does not steal focus moved to another control, and profile focus survives its refresh", async () => {
  let resolvePost;
  const ui = load({ post: () => new Promise(resolve => { resolvePost = resolve; }) });
  await settle();
  ui.setFocused("/api/pvmodel/reset");
  ui.click("/api/pvmodel/reset");
  ui.setFocusedProfile("away");
  resolvePost({ ok: true, status: 200, json: async () => ({}) });
  await settle();
  assert.equal(ui.focused(), "profile:away", "a user focus move wins over automatic restore");

  ui.setFocusedProfile("away");
  ui.clickProfile("away");
  await settle();
  assert.equal(ui.focused(), "profile:away", "profile changes retain keyboard focus after their GET repaint");
});

test("an unavailable model has no action, and an aborted request does not claim it started learning", async () => {
  const unavailable = load({ pv: { enabled: true, learning: { engine: "energyplan", status: "unavailable", started_ms: 0, latest_training_ms: 0, reset_available: false } } });
  await settle();
  assert.match(unavailable.grid.innerHTML, /Relearning is unavailable for this model/);
  assert.match(unavailable.grid.innerHTML, /data-reset-twin="\/api\/pvmodel\/reset"[^>]*disabled/);

  const aborted = load({ post: () => Promise.reject(Object.assign(new Error("offline"), { name: "AbortError" })) });
  await settle();
  aborted.click("/api/loadmodel/reset");
  await settle();
  assert.match(aborted.grid.innerHTML, /request was cancelled/i);
  assert.doesNotMatch(aborted.grid.innerHTML, /started learning/i);
});

test("a refused relearn reports the box response and never claims that it began", async () => {
  const ui = load({
    post: () => Promise.resolve({ ok: false, status: 409, json: async () => ({ error: "solar meter is stale" }) }),
  });
  await settle();
  ui.click("/api/pvmodel/reset");
  await settle();

  assert.match(ui.grid.innerHTML, /did not confirm a new learning period/i);
  assert.match(ui.grid.innerHTML, /HTTP 409.*solar meter is stale/i);
  assert.doesNotMatch(ui.grid.innerHTML, /The box accepted the request/);
});

test("a saved reset with a pending worker restart is retried from the GET state", async () => {
  const retryModel = model("unavailable");
  retryModel.learning.reset_available = true;
  const ui = load({
    pv: retryModel,
    post: () => Promise.resolve({
      ok: false,
      status: 503,
      json: async () => ({
        status: "pending",
        error: "Learning period saved; model restart pending: worker is unavailable",
        learning: retryModel.learning,
      }),
    }),
  });
  await settle();
  assert.doesNotMatch(ui.grid.innerHTML, /data-reset-twin="\/api\/pvmodel\/reset"[^>]*disabled/);

  ui.click("/api/pvmodel/reset");
  await settle();

  assert.equal(ui.requests.filter(request => request.path === "/api/pvmodel").length, 2, "pending response must refresh model state");
  assert.match(ui.grid.innerHTML, /Learning period saved; model restart pending: worker is unavailable/);
  assert.doesNotMatch(ui.grid.innerHTML, /did not confirm a new learning period/i);
  assert.doesNotMatch(ui.grid.innerHTML, /data-reset-twin="\/api\/pvmodel\/reset"[^>]*disabled/);
});
