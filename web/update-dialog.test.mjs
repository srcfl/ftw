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
      this.actions = [...html.matchAll(/data-action="([^"]*)"/g)].map(([, action]) => ({
        dataset: { action }, addEventListener(type, handler) { this[type] = handler; },
      }));
    }
    get innerHTML() { return this.html || ""; }
    querySelector(selector) {
      return selector === ".modal" ? this.modal : null;
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
  const requests = [];
  let Badge;
  const sandbox = {
    HTMLElement: Element,
    document: { body, createElement: () => new Element() },
    customElements: { define: (_, cls) => { Badge = cls; } },
    CustomEvent: class {},
    window: { location: { href: "http://127.0.0.1:8080/" } },
    fetch: (url, options) => {
      requests.push({ url, options });
      if (get) return Promise.resolve(get(url));
      return Promise.resolve({ ok: true, json: async () => ({}) });
    },
    setTimeout, clearTimeout, setInterval: () => 1, clearInterval() {},
    URL, console,
  };
  vm.runInNewContext(source, sandbox);
  const badge = new Badge();
  badge._phase = "dialog";
  badge._info = { native: true, current: "v0.131.0-beta.1", channel: "beta" };
  badge._render();
  return {
    badge, body, requests,
    root: () => body.children[0]?.shadowRoot || badge._shadow,
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

test("a native install reports its version and names ftw update, with no controls", () => {
  const rig = fixture();
  rig.badge._info = {
    native: true, current: "v0.131.0-beta.1", previous: "v0.130.4", channel: "beta",
    update_available: true, latest: "v0.131.0-beta.2",
    release_notes_url: "https://github.com/srcfl/ftw/releases/tag/v0.131.0-beta.2",
  };
  rig.badge._lastRun = { state: "failed", action: "update", message: "New Core did not reach readiness; previous Core is running" };
  rig.badge._render();
  const html = rig.root().innerHTML.split("</style>").pop();
  assert.match(html, /<strong>v0\.131\.0-beta\.1<\/strong> is running on the beta channel/);
  assert.match(html, /<strong>v0\.131\.0-beta\.2<\/strong> is published/);
  assert.match(html, /<pre class="cmd">ftw update<\/pre>/);
  assert.match(html, /Last update failed: New Core did not reach readiness/);
  assert.match(html, /What's new/);
  assert.deepEqual([...new Set(rig.root().actions.map(action => action.dataset.action))].sort(), ["check", "close"]);
  assert.doesNotMatch(html, /Return to v0\.130\.4|Offline restore|Snapshots|Full backup|Channel/);
});

test("a native release that changes stored data is named without a command", () => {
  const rig = fixture();
  rig.badge._info = {
    native: true, current: "v0.132.2", channel: "stable", update_available: true,
    latest: "v0.133.0", full_backup_required: true,
  };
  rig.badge._render();
  assert.match(rig.root().innerHTML, /changes stored data, which a native update cannot take yet/);
  assert.doesNotMatch(rig.root().innerHTML, /class="cmd"/);
});

test("a native release that failed on this machine is not offered again", () => {
  const rig = fixture();
  rig.badge._info = {
    native: true, current: "v0.135.0-beta.1", channel: "beta", update_available: true,
    latest: "v0.135.0-beta.2", last_failed: "v0.135.0-beta.2",
  };
  rig.badge._render();
  assert.match(rig.root().innerHTML, /It failed on this machine, so FTW waits for a newer release/);
  assert.doesNotMatch(rig.root().innerHTML, /class="cmd"/);
});

test("the dialog waits for the version check before reporting a version", () => {
  const rig = fixture();
  rig.badge._info = null;
  rig.badge._render();
  assert.match(rig.root().innerHTML, /Reading the running version/);
  assert.doesNotMatch(rig.root().innerHTML, /is running/);
});

test("the dialog offers no update, restart, rollback, channel or backup control", () => {
  const rig = fixture();
  rig.badge._info = { native: true, current: "v0.131.0-beta.1", update_available: true, latest: "v0.131.0-beta.2" };
  rig.badge._render();
  assert.deepEqual([...new Set(rig.root().actions.map(action => action.dataset.action))].sort(), ["check", "close"]);
  for (const route of ["/api/version/update", "/api/version/restart", "/api/version/binary-rollback",
    "/api/version/rollback", "/api/version/channel", "/api/version/skip", "/api/version/snapshots", "/api/backups"]) {
    // The status route is read, never a mutation.
    assert.doesNotMatch(source, new RegExp('"' + route + "(?!/status)"), route);
  }
});

test("opening reads only the version, the last run and the components", async () => {
  const rig = fixture({ get: () => ({ ok: true, json: async () => ({ state: "idle" }) }) });
  rig.requests.length = 0;
  rig.badge.open();
  await settled();
  assert.deepEqual(rig.requests.map(request => request.url).sort(),
    ["/api/components", "/api/version/check", "/api/version/update/status"]);
});

test("the header mark counts only Core; driver versions live in Settings › Devices", () => {
  const rig = fixture();
  rig.badge._info = { native: true, update_available: false };
  assert.equal(rig.badge._pendingUpdates().total, 0);
  rig.badge._info = { update_available: true };
  assert.equal(rig.badge._pendingUpdates().total, 1);
});

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

test("opening the dialog follows an update started with ftw update", async () => {
  const running = { state: "pulling", action: "update", target: "v0.131.0-beta.2", started_at: new Date().toISOString(),
    step: 1, total_steps: 3, progress_unit: "bytes", progress_current: 1048576, progress_total: 4194304 };
  const rig = fixture({ get: url => ({ ok: true, json: async () => url.endsWith("/update/status") ? running : {} }) });
  rig.badge.open();
  await settled();
  assert.equal(rig.badge._phase, "updating");
  assert.equal(rig.badge._runStatus.state, "pulling");
  assert.equal(rig.badge._expectedRun.target, running.target);
  const html = rig.root().innerHTML;
  assert.match(html, /Step 1 of 3 · Downloading release package/);
  assert.match(html, /1\.0 MB \/ 4\.0 MB/);
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
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
