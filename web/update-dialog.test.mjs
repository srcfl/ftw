import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const settled = () => new Promise(resolve => setImmediate(resolve));

function fixture({ get, storage = new Map() } = {}) {
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
  const timeouts = new Map();
  let timerID = 0;
  let Badge;
  const sandbox = {
    HTMLElement: Element,
    document: { body, createElement: () => new Element() },
    customElements: { define: (_, cls) => { Badge = cls; } },
    CustomEvent: class {},
    window: { location: { href: "http://127.0.0.1:8080/", replace() {} }, sessionStorage: {
      getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value),
      removeItem: key => storage.delete(key),
    } },
    fetch: (url, options) => {
      requests.push({ url, options });
      if (get) return Promise.resolve().then(() => get(url, options));
      return Promise.resolve({ ok: true, json: async () => ({}) });
    },
    setTimeout: (callback, delay) => { const id = ++timerID; timeouts.set(id, { callback, delay }); return id; },
    clearTimeout: id => timeouts.delete(id), setInterval: () => 1, clearInterval() {},
    URL, console, AbortController,
  };
  vm.runInNewContext(source, sandbox);
  const badge = new Badge();
  badge._phase = "dialog";
  badge._info = { native: true, current: "v0.131.0-beta.1", channel: "beta" };
  badge._render();
  return {
    badge, body, requests, storage, timeouts,
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

test("a ready native install offers an update alongside its running version and last result", () => {
  const rig = fixture();
  rig.badge._info = {
    native: true, install_ready: true, current: "v0.131.0-beta.1", previous: "v0.130.4", channel: "beta",
    update_available: true, latest: "v0.131.0-beta.2",
    release_notes_url: "https://github.com/srcfl/ftw/releases/tag/v0.131.0-beta.2",
  };
  rig.badge._lastRun = { state: "failed", action: "update", message: "New Core did not reach readiness; previous Core is running" };
  rig.badge._render();
  const html = rig.root().innerHTML.split("</style>").pop();
  assert.match(html, /<strong>v0\.131\.0-beta\.1<\/strong> is running on the beta channel/);
  assert.match(html, /<strong>v0\.131\.0-beta\.2<\/strong> is published/);
  assert.match(html, /data-action="update" >Update FTW/);
  assert.match(html, /briefly restart/);
  assert.match(html, /Last update failed: New Core did not reach readiness/);
  assert.match(html, /What's new/);
  assert.deepEqual([...new Set(rig.root().actions.map(action => action.dataset.action))].sort(), ["check", "close", "update"]);
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

test("a failed native release offers an explicit retry instead of an ordinary update", () => {
  const rig = fixture();
  rig.badge._info = {
    native: true, install_ready: true, current: "v0.135.0-beta.1", channel: "beta", update_available: true,
    latest: "v0.135.0-beta.2", last_failed: "v0.135.0-beta.2",
  };
  rig.badge._render();
  assert.match(rig.root().innerHTML, /FTW will only try it again when you ask/);
  assert.match(rig.root().innerHTML, /data-action="retry-update" >Try this version again/);
  assert.doesNotMatch(rig.root().innerHTML, /data-action="update"/);
  assert.doesNotMatch(rig.root().innerHTML, /class="cmd"/);
});

test("the dialog waits for the version check before reporting a version", () => {
  const rig = fixture();
  rig.badge._info = null;
  rig.badge._render();
  assert.match(rig.root().innerHTML, /Reading the running version/);
  assert.doesNotMatch(rig.root().innerHTML, /is running/);
});

test("non-native installs have no install control, and native UI adds only update", () => {
  const rig = fixture();
  rig.badge._info = { native: false, current: "v0.131.0-beta.1", update_available: true, latest: "v0.131.0-beta.2" };
  rig.badge._render();
  assert.deepEqual([...new Set(rig.root().actions.map(action => action.dataset.action))].sort(), ["check", "close"]);
  for (const route of ["/api/version/restart", "/api/version/binary-rollback",
    "/api/version/rollback", "/api/version/channel", "/api/version/skip", "/api/version/snapshots", "/api/backups"]) {
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
  assert.match(html, /1\.0 MB \/ 4\.2 MB/);
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
  await settled();
  const close = rig.root().actions.find(action => action.dataset.action === "close");
  close.click({ currentTarget:close });
  finishStatus({ ok:true, json:async () => ({state:"checking", action:"update", started_at:new Date().toISOString()}) });
  await settled();
  assert.equal(rig.badge._phase, "idle");
  assert.equal(rig.body.children.length, 0);
});

function offer(rig, extra = {}) {
  rig.badge._info = { native: true, install_ready: true, update_available: true,
    current: "v0.131.0-beta.1", latest: "v0.131.0-beta.2", ...extra };
  rig.badge._render();
}

function click(rig, action) {
  const button = rig.root().actions.find(el => el.dataset.action === action);
  assert.ok(button, action);
  button.click({ currentTarget: button });
}

const reply = body => ({ ok: true, json: async () => body });
const run = (state, started = new Date().toISOString()) => ({ state, action: "update", component: "core",
  target: "v0.131.0-beta.2", started_at: started, step: 1, total_steps: 3 });

test("the update button sends one request and binds progress to the accepted server run", async () => {
  let accept;
  let posted = false;
  const serverStart = "2020-01-01T00:00:00Z"; // clocks differ; accepted identity still wins
  const rig = fixture({ get: (url, options) => {
    if (options?.method === "POST") { posted = true; return new Promise(resolve => { accept = resolve; }); }
    return reply(posted ? run("pulling", serverStart) : { state: "idle" });
  } });
  offer(rig);
  click(rig, "update");
  rig.badge._startUpdate();
  await settled();
  const posts = rig.requests.filter(r => r.options?.method === "POST");
  assert.equal(posts.length, 1);
  assert.equal(posts[0].url, "/api/version/update");
  assert.equal(posts[0].options.body, undefined);
  assert.equal(rig.badge._phase, "updating");
  assert.match(rig.badge._shadow.innerHTML, /FTW update in progress/);
  accept(reply({ status: "started", action: "update", target: "v0.131.0-beta.2", started_at: serverStart }));
  await settled();
  assert.equal(rig.badge._runStatus.state, "pulling");
  assert.equal(rig.badge._expectedRun.started_at, serverStart);
  assert.equal(rig.badge._statusMatchesCurrentRun(run("done", "2019-12-31T23:59:59Z")), false);
  assert.equal(rig.badge._statusMatchesCurrentRun({ ...run("done", serverStart), target: "another-version" }), false);
});

test("a schema change, missing slots, stale check or lost contact cannot trigger installation", async () => {
  for (const info of [{ full_backup_required: true }, { install_ready: false }, { err: "GitHub unavailable" }, { native: false }]) {
    const rig = fixture();
    offer(rig, info);
    await rig.badge._startUpdate();
    assert.equal(rig.requests.length, 0);
  }
  const rig = fixture();
  offer(rig);
  rig.badge.setConnected(false);
  assert.match(rig.root().innerHTML, /was last reported by the box/);
  assert.match(rig.root().innerHTML, /data-action="update" disabled/);
  await rig.badge._startUpdate();
  assert.equal(rig.requests.length, 0);
});

test("retrying a failed release requires the explicit retry action", async () => {
  let posted = false;
  const rig = fixture({ get: (url, options) => {
    if (options?.method === "POST") { posted = true; return reply({ status: "started", target: "v0.131.0-beta.2" }); }
    return reply(posted ? run("pulling") : { state: "idle" });
  } });
  offer(rig, { last_failed: "v0.131.0-beta.2" });
  await rig.badge._startUpdate();
  assert.equal(rig.requests.length, 0);
  click(rig, "retry-update");
  await settled();
  const posts = rig.requests.filter(r => r.options?.method === "POST");
  assert.equal(posts.length, 1);
  assert.deepEqual(JSON.parse(posts[0].options.body), { retry: true });
});

test("a refused request shows the escaped reason and never pretends the update started", async () => {
  const rig = fixture({ get: (url, options) => options?.method === "POST"
    ? { ok: false, json: async () => ({ error: "Not enough space <script>" }) }
    : reply(url.endsWith("/update/status") ? { state: "idle" } : { native: true, current: "v0.131.0-beta.1" }) });
  offer(rig);
  click(rig, "update");
  await settled();
  assert.equal(rig.badge._phase, "dialog");
  assert.match(rig.root().innerHTML, /role="alert">Not enough space &lt;script&gt;/);
  assert.equal(rig.storage.size, 0);
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 1);
});

test("a lost POST reply follows saved progress without sending another request", async () => {
  let posted = false;
  const rig = fixture({ get: (url, options) => {
    if (options?.method === "POST") { posted = true; throw new Error("reply lost during restart"); }
    return reply(posted ? run("restarting") : { state: "idle" });
  } });
  offer(rig);
  click(rig, "update");
  await settled();
  assert.equal(rig.badge._runStatus.state, "restarting");
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 1);
  assert.equal(rig.badge._phase, "updating");
});

test("a preflight read follows an existing update without posting another", async () => {
  const rig = fixture({ get: () => reply(run("pulling")) });
  offer(rig);
  click(rig, "update");
  await settled();
  assert.equal(rig.badge._runStatus.state, "pulling");
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
});

test("reloading an unconfirmed request with no saved run shows uncertainty without retrying", async () => {
  const storage = new Map([["ftw-native-update-run", JSON.stringify({ requested_at: Date.now(),
    run: { action: "update", target: "", baseline_checked: true } })]]);
  const rig = fixture({ storage, get: url => reply(url.endsWith("/update/status")
    ? { state: "idle" } : { native: true, current: "v0.131.0-beta.1" }) });
  rig.badge.connectedCallback();
  await settled();
  assert.equal(rig.badge._phase, "dialog");
  assert.match(rig.root().innerHTML, /no matching update report/);
  assert.equal(storage.size, 0);
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
});

test("lost contact keeps the last measured report and reconnects without reinstalling", async () => {
  let offline = true;
  const rig = fixture({ get: () => { if (offline) throw new Error("restarting"); return reply(run("checking")); } });
  rig.badge._phase = "updating";
  rig.badge._updateStartedAt = Date.now();
  rig.badge._expectedRun = { action: "update", target: "v0.131.0-beta.2" };
  rig.badge._runStatus = { ...run("pulling"), progress_unit: "bytes", progress_current: 0, progress_total: 4194304 };
  rig.badge._tickStatus();
  await settled();
  assert.match(rig.root().innerHTML, /Cannot reach the box/);
  assert.match(rig.root().innerHTML, /max="4194304" value="0"/);
  assert.match(rig.root().innerHTML, /0 B \/ 4\.2 MB/);
  offline = false;
  rig.badge._tickStatus();
  await settled();
  assert.doesNotMatch(rig.root().innerHTML, /Cannot reach the box/);
  assert.equal(rig.badge._runStatus.state, "checking");
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
});

test("reloading a completed UI update opens its result once and reads the actual running version", async () => {
  const storage = new Map([["ftw-native-update-run", JSON.stringify({ requested_at: Date.now(),
    run: { action: "update", target: "v0.131.0-beta.2", started_at: "2020-01-01T00:00:00Z" } })]]);
  const rig = fixture({ storage, get: url => reply(url.endsWith("/update/status")
    ? run("done", "2020-01-01T00:00:00Z") : { native: true, current: "v0.131.0-beta.2" }) });
  rig.badge.connectedCallback();
  await settled();
  assert.equal(rig.badge._phase, "dialog");
  assert.match(rig.root().innerHTML, /v0\.131\.0-beta\.2<\/strong> is running/);
  assert.match(rig.root().innerHTML, /Last update:/);
  assert.equal(storage.size, 0);
  assert.equal([...rig.timeouts.values()].some(timer => timer.delay === 800), false, "no reload loop");
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
});

test("a failed trial reports the actual fallback version and the saved failure", async () => {
  const started = new Date().toISOString();
  const rig = fixture({ get: url => reply(url.endsWith("/update/status")
    ? { ...run("failed", started), message: "Trial failed; previous Core is running" }
    : { native: true, current: "v0.131.0-beta.1", latest: "v0.131.0-beta.2", update_available: true, install_ready: true, last_failed: "v0.131.0-beta.2" }) });
  rig.badge._phase = "updating";
  rig.badge._expectedRun = { action: "update", target: "v0.131.0-beta.2", started_at: started };
  rig.badge._tickStatus();
  await settled();
  assert.equal(rig.badge._phase, "dialog");
  assert.match(rig.root().innerHTML, /v0\.131\.0-beta\.1<\/strong> is running/);
  assert.match(rig.root().innerHTML, /Last update failed: Trial failed; previous Core is running/);
  assert.match(rig.root().innerHTML, /Try this version again/);
  assert.equal(rig.requests.filter(r => r.options?.method === "POST").length, 0);
});

test("unpacking shows known completed steps but no percentage or health claim", () => {
  const rig = fixture();
  rig.badge._phase = "updating";
  rig.badge._updateStartedAt = Date.now();
  rig.badge._runStatus = { ...run("checking"), step: 2, phases: [{ message: "Downloaded release", started_at: "2026-10-07T06:00:00Z", finished_at: "2026-10-07T06:00:04Z" }] };
  rig.badge._render();
  assert.match(rig.root().innerHTML, /Step 2 of 3 · Unpacking and checking release/);
  assert.match(rig.root().innerHTML, /does not report a work total/);
  assert.match(rig.root().innerHTML, /Downloaded release · 4s/);
  assert.doesNotMatch(rig.root().innerHTML, /Core has started|aria-valuenow|value="[0-9]/);
});
