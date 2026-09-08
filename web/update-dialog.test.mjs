import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const settled = () => new Promise(resolve => setImmediate(resolve));

function fixture({ get } = {}) {
  class Root {
    set innerHTML(html) {
      this.html = html;
      this.modal = /<div class="modal"/.test(html) ? { scrollTop: 0 } : null;
      const storage = html.match(/<details class="snapshots storage"([^>]*)>/);
      this.storage = storage ? { open: /\bopen\b/.test(storage[1]) } : null;
      this.actions = [...html.matchAll(/data-action="([^"]*)"/g)].map(([, action]) => ({
        dataset: { action }, addEventListener(type, handler) { this[type] = handler; },
      }));
    }
    get innerHTML() { return this.html || ""; }
    querySelector(selector) {
      return selector === ".modal" ? this.modal : selector === "details.storage" ? this.storage : null;
    }
    querySelectorAll(selector) { return selector === "[data-action]" ? this.actions || [] : []; }
  }
  class Element {
    constructor() { this.children = []; this.isConnected = true; }
    attachShadow() { this.shadowRoot = new Root(); return this.shadowRoot; }
    appendChild(child) { child.parentElement = this; this.children.push(child); }
    remove() { this.parentElement.children = this.parentElement.children.filter(child => child !== this); }
    dispatchEvent() {}
  }
  const body = new Element();
  const requests = [], alerts = [];
  let Badge, finish;
  const sandbox = {
    HTMLElement: Element,
    document: { body, createElement: () => new Element() },
    customElements: { define: (_, cls) => { Badge = cls; } },
    CustomEvent: class {},
    window: { alert: message => alerts.push(message) },
    fetch: (url, options) => {
      requests.push({ url, options });
      if (options?.method === "POST") return new Promise(resolve => { finish = resolve; });
      if (get) return Promise.resolve(get(url));
      return Promise.resolve({ ok: true, json: async () => ({ enabled: true, backups: [], snapshots: [] }) });
    },
    setTimeout, clearTimeout, setInterval: () => 1, clearInterval() {},
    URL, console,
  };
  vm.runInNewContext(source, sandbox);
  const badge = new Badge();
  badge._phase = "dialog";
  badge._backups = { enabled: true, backups: [], on_device: true };
  badge._snapshots = { enabled: true, snapshots: [] };
  badge._render();
  return {
    badge, body, requests, alerts,
    root: () => body.children[0]?.shadowRoot || badge._shadow,
    finish: ok => finish({ ok, json: async () => ok ? {} : { error: "fixture failed" } }),
  };
}

test("the dialog and update progress render outside the header badge", () => {
  const rig = fixture();
  assert.equal(rig.badge._shadow.querySelector(".modal"), null);
  assert.equal(rig.body.children.length, 1);
  assert.ok(rig.root().querySelector(".modal"));
  rig.badge._phase = "updating";
  rig.badge._render();
  assert.equal(rig.body.children.length, 1, "renders reuse one overlay host");
  assert.ok(rig.root().querySelector(".modal"));
  rig.badge._phase = "idle";
  rig.badge._render();
  assert.equal(rig.body.children.length, 0, "closing removes the overlay");
});

for (const [method, path] of [["_createBackup", "/api/backups"], ["_createSnapshot", "/api/version/snapshots"]]) {
  for (const ok of [true, false]) {
    test(method + " keeps the open section and scroll through progress and " + (ok ? "success" : "failure"), async () => {
      const rig = fixture();
      rig.root().storage.open = true;
      rig.root().modal.scrollTop = 270;
      rig.badge[method]();
      assert.equal(rig.requests[0].url, path);
      assert.equal(rig.root().storage.open, true, "progress must stay visible");
      assert.equal(rig.root().modal.scrollTop, 270);
      assert.match(rig.root().innerHTML, /Creating/);
      rig.badge.setConnected(false);
      assert.equal(rig.root().storage.open, true, "a concurrent status render must preserve the section");
      assert.equal(rig.root().modal.scrollTop, 270);
      rig.finish(ok);
      await settled();
      assert.equal(rig.root().storage.open, true, "completion must remain in view");
      assert.equal(rig.root().modal.scrollTop, 270);
      assert.equal(rig.alerts.length, ok ? 0 : 1);
    });
  }
}

test("disabling or removing the badge leaves no detached dialog", () => {
  for (const action of ["_disable", "disconnectedCallback"]) {
    const rig = fixture();
    if (action === "disconnectedCallback") rig.badge.isConnected = false;
    rig.badge[action]();
    assert.equal(rig.body.children.length, 0);
    rig.badge._render();
    assert.equal(rig.body.children.length, 0, "a late response must not recreate a detached dialog");
  }
});

test("opening Updates adopts work started by another client and blocks a duplicate start", async () => {
  const running = { state: "snapshotting", action: "update", target: "v3.2.1-beta.1", started_at: new Date().toISOString() };
  const rig = fixture({ get: url => ({ ok: true, json: async () => url.endsWith("/update/status") ? running : {} }) });
  rig.badge.open();
  assert.equal(rig.badge._checkingCurrentRun, true);
  rig.badge._beginUpdate("update");
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
  await settled();
  assert.equal(rig.badge._phase, "updating");
  assert.equal(rig.badge._sidecarState.state, "snapshotting");
  assert.equal(rig.badge._expectedRun.target, running.target);
});

test("a startup 503 keeps update progress available; only explicit disable hides it", async () => {
  for (const error of ["starting", "self-update disabled"]) {
    const rig = fixture({ get: () => ({ ok: false, status: 503, json: async () => ({ error }) }) });
    rig.badge._refresh(false);
    await settled();
    assert.equal(rig.badge._disabled, error === "self-update disabled");
    if (error === "starting") {
      rig.badge.open();
      assert.match(rig.root().innerHTML, /Control has not started yet/);
      assert.doesNotMatch(rig.root().innerHTML, /Update failed/);
    }
  }
});

test("a late status response does not reopen a dismissed dialog", async () => {
  let finishStatus;
  const rig = fixture({ get: url => url.endsWith("/update/status")
    ? new Promise(resolve => { finishStatus = resolve; })
    : { ok:true, json:async () => ({}) } });
  rig.badge.open();
  const close = rig.root().actions.find(action => action.dataset.action === "close");
  close.click({ currentTarget:close });
  finishStatus({ ok:true, json:async () => ({state:"checking", action:"update", started_at:new Date().toISOString()}) });
  await settled();
  assert.equal(rig.badge._phase, "idle");
  assert.equal(rig.body.children.length, 0);
});
