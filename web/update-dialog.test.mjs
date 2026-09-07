import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const settled = () => new Promise(resolve => setImmediate(resolve));

function fixture() {
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
