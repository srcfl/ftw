import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The settings shell owns save, and render() runs only when a tab is opened or
// switched to. So a tab that shows what the BOX says about itself — rather
// than what the form holds — keeps showing the pre-save answer unless the
// shell calls it back. The fleet ping tab is the one that does, and the line
// it prints is a claim about whether anything is being sent.
//
// settings.js is a plain IIFE over a handful of elements, so it loads under a
// small DOM shim and this test drives the real save handler.

const source = readFileSync(new URL("./settings.js", import.meta.url), "utf8");

const ELEMENT_IDS = [
  "settings-modal", "settings-btn", "settings-close",
  "settings-save", "settings-status", "settings-tabs", "settings-body",
];

function stubElement(ownerDocument = {}) {
  const classes = new Set();
  return {
    textContent: "",
    className: "",
    innerHTML: "",
    dataset: {},
    style: {},
    handlers: {},
    attributes: {},
    isConnected: true,
    tabIndex: 0,
    classList: {
      add: name => classes.add(name), remove: name => classes.delete(name),
      contains: name => classes.has(name), toggle() {},
    },
    setAttribute(name, value) { this.attributes[name] = value; },
    focus() { ownerDocument.activeElement = this; },
    getClientRects() { return this.hidden ? [] : [{}]; },
    matches(selector) { return selector === ":disabled" && !!this.disabled; },
    contains(el) {
      for (; el; el = el.parentElement) if (el === this) return true;
      return false;
    },
    addEventListener(type, fn) { this.handlers[type] = fn; },
    querySelectorAll: () => [],
    appendChild() {},
  };
}

function loadShell(saveResponse, ok = true) {
  const elements = {};
  const document = {
    getElementById: id => elements[id] || null,
    createElement: () => stubElement(document),
  };
  for (const id of ELEMENT_IDS) elements[id] = stubElement(document);
  elements["settings-modal"].classList.add("hidden");

  const requests = [];
  const responses = {};
  const alerts = [];
  const sandbox = {
    window: { FTWSettings: { tabs: {} } },
    document,
    getComputedStyle: element => ({ visibility: element.visibility || "visible" }),
    alert: message => alerts.push(message),
    fetch(path, opts) {
      requests.push({ path, opts });
      if (typeof responses[path] === "function") return responses[path](opts);
      return Promise.resolve({ ok, status: ok ? 200 : 400, headers: { get: () => '"config-7"' }, json: () => Promise.resolve(responses[path] ?? saveResponse) });
    },
    // The shell only uses timers to clear the "Saved" status and to poll after
    // a restart; neither is what these tests are about.
    setTimeout: () => 0,
    Date, JSON, Array, Object, String, console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return { elements, document, requests, responses, alerts, tabs: sandbox.window.FTWSettings.tabs,
    loadTab: file => vm.runInContext(readFileSync(new URL(file, import.meta.url), "utf8"), sandbox) };
}

// One turn of the event loop, which is all the save chain needs to settle.
const settled = () => new Promise((resolve) => setImmediate(resolve));

describe("Settings dialog keyboard access", () => {
  async function open(rig, opener = rig.elements["settings-btn"]) {
    opener.focus();
    rig.elements["settings-btn"].handlers.click();
    await settled();
  }

  function key(rig, name, properties = {}) {
    const event = { key: name, preventDefault() { this.defaultPrevented = true; },
      stopPropagation() { this.stopped = true; }, ...properties };
    rig.elements["settings-modal"].handlers.keydown?.(event);
    return event;
  }

  it("focuses the dialog after loading and Escape returns to the actual opener", async () => {
    const rig = loadShell({});
    const more = stubElement(rig.document);
    more.focus();
    rig.elements["settings-btn"].handlers.click();
    assert.equal(rig.document.activeElement, more, "loading does not focus the hidden dialog");
    stubElement(rig.document).focus();
    await settled();
    assert.equal(rig.document.activeElement, rig.elements["settings-close"]);
    assert.deepEqual(rig.elements["settings-modal"].attributes,
      { role: "dialog", "aria-modal": "true", "aria-label": "Settings" });
    const event = key(rig, "Escape");
    assert.equal(rig.elements["settings-modal"].classList.contains("hidden"), true);
    assert.equal(rig.document.activeElement, more, "More delegates a click to the hidden header button");
    assert.ok(event.defaultPrevented && event.stopped);
  });

  it("returns focus after close or backdrop click and keeps inside clicks open", async () => {
    const rig = loadShell({});
    const modal = rig.elements["settings-modal"];
    await open(rig);
    modal.handlers.click({ target: rig.elements["settings-body"] });
    assert.equal(modal.classList.contains("hidden"), false);
    rig.elements["settings-close"].handlers.click();
    assert.equal(rig.document.activeElement, rig.elements["settings-btn"]);
    const nextOpener = stubElement(rig.document);
    await open(rig, nextOpener);
    modal.handlers.click({ target: modal });
    assert.equal(modal.classList.contains("hidden"), true);
    assert.equal(rig.document.activeElement, nextOpener);
  });

  it("wraps Tab in both directions and skips hidden, disabled and untabbable controls", async () => {
    const rig = loadShell({});
    const first = rig.elements["settings-close"], last = stubElement(rig.document);
    const unavailable = [{ hidden: true }, { disabled: true }, { tabIndex: -1 }, { visibility: "hidden" }]
      .map(properties => Object.assign(stubElement(rig.document), properties));
    rig.elements["settings-modal"].querySelectorAll = () => [first, last, ...unavailable];
    await open(rig);
    assert.ok(key(rig, "Tab", { shiftKey: true }).defaultPrevented);
    assert.equal(rig.document.activeElement, last);
    assert.ok(key(rig, "Tab").defaultPrevented);
    assert.equal(rig.document.activeElement, first);
    assert.equal(key(rig, "Tab").defaultPrevented, undefined, "ordinary Tab stays native");
    assert.equal(key(rig, "Enter").defaultPrevented, undefined);
  });

  it("leaves a handled Escape alone and stops handling keys when closed", async () => {
    const rig = loadShell({});
    await open(rig);
    key(rig, "Escape", { defaultPrevented: true });
    assert.equal(rig.elements["settings-modal"].classList.contains("hidden"), false);
    rig.elements["settings-close"].handlers.click();
    assert.equal(key(rig, "Tab").defaultPrevented, undefined);
  });

  it("uses summaries of closed details and includes their controls after opening", async () => {
    const rig = loadShell({}), modal = rig.elements["settings-modal"], first = rig.elements["settings-close"];
    const summary = stubElement(rig.document), innerSummary = stubElement(rig.document), field = stubElement(rig.document);
    const details = { tagName: "DETAILS", open: false, parentElement: modal, querySelector: () => summary };
    const nested = { tagName: "DETAILS", open: false, parentElement: details, querySelector: () => innerSummary };
    summary.parentElement = details;
    summary.contains = el => el === summary;
    innerSummary.parentElement = nested;
    innerSummary.contains = el => el === innerSummary;
    field.parentElement = details;
    modal.querySelectorAll = () => [first, summary, innerSummary, field];
    await open(rig);
    key(rig, "Tab", { shiftKey: true });
    assert.equal(rig.document.activeElement, summary);
    key(rig, "Tab");
    assert.equal(rig.document.activeElement, first);
    details.open = true;
    key(rig, "Tab", { shiftKey: true });
    assert.equal(rig.document.activeElement, field, "recompute controls on each keypress");
    key(rig, "Tab");
    assert.equal(rig.document.activeElement, first);
  });

  it("does not focus an opener that was removed or hidden", async () => {
    for (const properties of [{ isConnected: false }, { hidden: true }]) {
      const rig = loadShell({}), opener = stubElement(rig.document);
      await open(rig, opener);
      Object.assign(opener, properties);
      key(rig, "Escape");
      assert.notEqual(rig.document.activeElement, opener);
      assert.equal(rig.elements["settings-modal"].classList.contains("hidden"), true);
    }
  });

  it("keeps focus in Settings when an in-tab action replaces its own button", async () => {
    const rig = loadShell({}), body = rig.elements["settings-body"], button = stubElement(rig.document);
    const selectedTab = stubElement(rig.document);
    body.parentElement = selectedTab.parentElement = rig.elements["settings-modal"];
    button.parentElement = body;
    let context;
    rig.tabs.control = { render: ctx => { context = ctx; return ""; } };
    rig.tabs.devices = { render: () => "" };
    rig.elements["settings-tabs"].querySelector = () => selectedTab;
    Object.defineProperty(body, "innerHTML", { set() {
      if (body.contains(rig.document.activeElement)) rig.document.activeElement = null;
    } });
    await open(rig);
    button.focus();
    context.navigateTab("devices");
    assert.equal(rig.document.activeElement, selectedTab);
    rig.tabs.devices.after = () => button.focus();
    button.focus();
    context.renderTab("devices");
    assert.equal(rig.document.activeElement, button, "keep explicit focus from the new tab's hook");
  });

  async function restartShell() {
    const rig = loadShell({ restart_required: true });
    for (const id of ["restart-modal", "restart-reasons", "restart-later", "restart-now", "restart-progress", "restart-progress-text"])
      rig.elements[id] = stubElement(rig.document);
    rig.elements["restart-modal"].classList.add("hidden");
    const dialog = stubElement(rig.document), background = stubElement(rig.document), alreadyInert = stubElement(rig.document);
    alreadyInert.inert = true;
    rig.elements["restart-modal"].querySelector = () => dialog;
    rig.document.body = { children: [rig.elements["settings-modal"], background, alreadyInert, rig.elements["restart-modal"]] };
    await open(rig);
    rig.elements["settings-save"].focus();
    rig.elements["settings-save"].handlers.click();
    await settled();
    return { ...rig, dialog, background, alreadyInert };
  }

  function restartKey(rig, name, properties = {}) {
    const event = { key: name, preventDefault() { this.defaultPrevented = true; },
      stopPropagation() { this.stopped = true; }, ...properties };
    rig.elements["restart-modal"].onkeydown?.(event);
    return event;
  }

  it("hands focus to Restart later and returns it to Save when that dialog closes", async () => {
    const rig = await restartShell();
    assert.equal(rig.document.activeElement, rig.elements["restart-later"]);
    rig.elements["restart-later"].onclick();
    assert.equal(rig.document.activeElement, rig.elements["settings-save"]);
    assert.equal(rig.elements["settings-modal"].classList.contains("hidden"), false);
    assert.equal(rig.requests.some(request => request.path === "/api/restart"), false);
  });

  it("keeps the restart prompt modal and wraps focus until Escape chooses Later", async () => {
    const rig = await restartShell();
    assert.equal(rig.dialog.attributes.role, "dialog");
    assert.equal(rig.dialog.attributes["aria-modal"], "true");
    assert.equal(rig.dialog.attributes["aria-label"], "Restart required");
    assert.equal(rig.background.inert, true);
    assert.equal(rig.elements["settings-modal"].inert, true);
    assert.notEqual(rig.elements["restart-modal"].inert, true);
    rig.elements["settings-save"].handlers.click();
    await settled();
    assert.equal(rig.document.activeElement, rig.elements["restart-later"]);
    for (const shiftKey of [false, true]) {
      assert.ok(restartKey(rig, "Tab", { shiftKey }).defaultPrevented);
      assert.equal(rig.document.activeElement, rig.elements["restart-now"]);
      restartKey(rig, "Tab", { shiftKey });
      assert.equal(rig.document.activeElement, rig.elements["restart-later"]);
    }
    restartKey(rig, "Escape", { defaultPrevented: true });
    assert.equal(rig.elements["restart-modal"].classList.contains("hidden"), false);
    assert.ok(restartKey(rig, "Escape").stopped);
    assert.equal(rig.elements["restart-modal"].classList.contains("hidden"), true);
    assert.equal(rig.elements["settings-modal"].classList.contains("hidden"), false);
    assert.equal(rig.document.activeElement, rig.elements["settings-save"]);
    assert.equal(rig.background.inert, false);
    assert.equal(rig.elements["settings-modal"].inert, false);
    assert.equal(rig.alreadyInert.inert, true, "leave pre-existing inert state alone");
    assert.equal(rig.elements["restart-modal"].onkeydown, null);
  });

  it("keeps pending restart focus in the prompt and permits Later after a failure", async () => {
    const rig = await restartShell();
    let finish;
    rig.responses["/api/restart"] = () => new Promise(resolve => { finish = resolve; });
    rig.elements["restart-now"].onclick();
    assert.equal(rig.document.activeElement, rig.dialog);
    assert.equal(rig.elements["restart-later"].disabled, true);
    assert.equal(rig.elements["restart-now"].disabled, true);
    assert.equal(rig.elements["restart-progress"].classList.contains("hidden"), false);
    rig.elements["settings-save"].handlers.click();
    await settled();
    assert.equal(rig.elements["restart-later"].disabled, true, "a late save response must not unlock the pending prompt");
    assert.equal(rig.document.activeElement, rig.dialog);
    for (const shiftKey of [false, true]) {
      assert.ok(restartKey(rig, "Tab", { shiftKey }).defaultPrevented);
      assert.equal(rig.document.activeElement, rig.dialog);
    }
    restartKey(rig, "Escape");
    rig.elements["restart-later"].onclick();
    assert.equal(rig.elements["restart-modal"].classList.contains("hidden"), false);
    assert.equal(rig.background.inert, true);
    finish({ ok: false, status: 500, json: async () => ({ error: "offline" }) });
    await settled();
    assert.deepEqual(rig.alerts, ["Restart failed: offline"]);
    assert.equal(rig.document.activeElement, rig.elements["restart-later"]);
    assert.equal(rig.elements["restart-progress"].classList.contains("hidden"), true);
    restartKey(rig, "Escape");
    assert.equal(rig.document.activeElement, rig.elements["settings-save"]);
    assert.equal(rig.background.inert, false);
    assert.equal(rig.requests.filter(request => request.path === "/api/restart").length, 1);
  });
});

async function formShell(original = { site: { name: "Home" }, planner: { enabled: true }, hidden: { keep: 17 } }) {
  const rig = loadShell(structuredClone(original));
  const field = (path, type, value) => Object.assign(stubElement(), { dataset: { path }, type, value });
  const fields = {
    min: field("planner.soc_min", "number", "0.1"),
    max: field("planner.soc_max", "number", "0.95"),
    engine: field("planner.engine", "select-one", ""),
    checkbox: Object.assign(stubElement(), { dataset: { checkboxPath: "optional.enabled" }, checked: false }),
    name: field("site.name", "text", "Home"),
  };
  let visible = [], context;
  rig.elements["settings-body"].querySelectorAll = selector => visible.filter(input =>
    selector === "[data-path]" ? input.dataset.path : selector === "[data-checkbox-path]" && input.dataset.checkboxPath);
  rig.tabs.control = { render: ctx => { context = ctx; visible = [fields.name]; return ""; } };
  rig.tabs.planner = { render: () => { visible = [fields.min, fields.max, fields.engine, fields.checkbox]; return ""; } };
  rig.elements["settings-btn"].handlers.click();
  await settled();
  return { ...rig, original, fields, context, addField: input => visible.push(input), saved: () => JSON.parse(rig.requests.filter(r => r.opts?.method === "POST").at(-1).opts.body) };
}

describe("unchanged settings fields", () => {
  for (const visitPlanner of [false, true]) {
    it("preserves absent values on save, visit Planner=" + visitPlanner, async () => {
      const rig = await formShell();
      if (visitPlanner) rig.context.navigateTab("planner");
      await rig.context.saveConfig();
      assert.deepEqual(rig.saved(), rig.original);
      assert.equal(rig.requests.at(-1).opts.headers["If-Match"], '"config-7"');
    });
  }

  it("keeps Planner defaults absent after visiting it and saving another tab", async () => {
    const rig = await formShell();
    rig.context.navigateTab("planner");
    rig.context.navigateTab("control");
    rig.fields.name.value = "New name";
    await rig.context.saveConfig();
    assert.deepEqual(rig.saved(), { ...rig.original, site: { name: "New name" } });
  });

  it("saves explicit number, select and checkbox edits and a later return to the rendered value", async () => {
    const rig = await formShell();
    rig.context.navigateTab("planner");
    rig.fields.max.value = "0.8";
    rig.fields.engine.value = "core";
    rig.fields.checkbox.checked = true;
    await rig.context.saveConfig();
    assert.deepEqual(rig.saved().planner, { enabled: true, soc_max: 0.8, engine: "core" });
    assert.deepEqual(rig.saved().optional, { enabled: true });
    rig.fields.max.value = "0.95";
    await rig.context.saveConfig();
    assert.equal(rig.saved().planner.soc_max, 0.95);
  });

  it("preserves a late secret input until its value changes", async () => {
    const rig = await formShell();
    const secret = Object.assign(stubElement(), { dataset: { path: "device.secret" }, type: "password", defaultValue: "", value: "" });
    rig.addField(secret);
    await rig.context.saveConfig();
    assert.deepEqual(rig.saved(), rig.original);
    secret.value = "local-test-value";
    await rig.context.saveConfig();
    assert.equal(rig.saved().device.secret, "local-test-value");
  });
});

// Run the real asynchronous Devices catalog callback. The shim parses the
// select it inserts; like HTMLSelectElement, it has no defaultValue property.
async function profileShell() {
  const original = { site: { name: "Home" }, drivers: [{ name: "GoodWe", lua: "drivers/goodwe.lua",
    capabilities: { modbus: { unit_id: 1 } }, config: {} }] };
  const rig = await formShell(original);
  rig.loadTab("./settings/tabs/devices.js");
  rig.responses["/api/drivers/catalog"] = { entries: [{ id: "goodwe", path: "drivers/goodwe.lua", version: "1.0.2" }] };
  const unit = Object.assign(stubElement(), { dataset: { path: "drivers.0.capabilities.modbus.unit_id" },
    type: "number", defaultValue: "1", value: "1" });
  rig.addField(unit);
  let select;
  const slot = {
    getAttribute: () => "0",
    set innerHTML(html) {
      const options = [...html.matchAll(/<option value="([^"]*)"([^>]*)>/g)]
        .map(([, value, attrs]) => ({ value, defaultSelected: /\bselected\b/.test(attrs) }));
      select = Object.assign(stubElement(), { type: "select-one", options,
        dataset: { path: html.match(/data-path="([^"]*)"/)[1] },
        value: (options.find(option => option.defaultSelected) || options[0]).value });
      rig.addField(select);
    },
    querySelector: () => select,
  };
  const body = rig.elements["settings-body"];
  const fields = body.querySelectorAll;
  body.querySelectorAll = selector => selector === ".drv-profile-slot" ? [slot] : fields(selector);
  body.querySelector = selector => selector === '[data-path="drivers.0.capabilities.modbus.unit_id"]' ? unit : null;
  rig.tabs.devices.after({ ...rig.context, escHtml: String, help: () => "" });
  assert.equal(select, undefined, "the catalog has not arrived during render");
  await settled();
  assert.equal("defaultValue" in select, false);
  return { ...rig, select, unit };
}

describe("late driver profile fields", () => {
  for (const switchTab of [false, true]) {
    it("keeps an untouched profile absent, switch tab=" + switchTab, async () => {
      const rig = await profileShell();
      if (switchTab) rig.context.navigateTab("control");
      await rig.context.saveConfig();
      assert.deepEqual(rig.saved(), rig.original);
    });
  }

  it("saves an explicit profile and its programmatic Unit ID, including a later return", async () => {
    const rig = await profileShell();
    for (const [profile, unitID] of [["gw8kn-et-hk3000", 247], ["community-v1", 1]]) {
      rig.select.value = profile;
      rig.select.handlers.change();
      assert.equal(rig.unit.value, String(unitID));
      await rig.context.saveConfig();
      assert.deepEqual(rig.saved().drivers[0], { ...rig.original.drivers[0],
        capabilities: { modbus: { unit_id: unitID } }, config: { profile } });
    }
  });
});

describe("the settings shell after a save", () => {
  it("calls the tab back so it can ask the box again", async () => {
    const { elements, tabs } = loadShell({ restart_required: false, restart_reasons: [] });
    let called = 0;
    // "control" is the tab the shell opens on.
    tabs.control = { render: () => "", afterSave: () => { called += 1; } };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(called, 1, "the tab was never told the save landed");
  });

  it("survives a tab that has no post-save hook", async () => {
    const { elements, tabs, requests } = loadShell({ restart_required: false, restart_reasons: [] });
    tabs.control = { render: () => "" };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, "/api/config");
  });

  it("does not call the hook when the save was rejected", async () => {
    // Refreshing after a rejected save would put the box's unchanged answer
    // under a "Save failed" banner, which reads as though it went through.
    const { elements, tabs } = loadShell({ error: "validation: nope" }, false);
    let called = 0;
    tabs.control = { render: () => "", afterSave: () => { called += 1; } };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(called, 0, "a rejected save told the tab it landed");
  });
});


describe("charger setup navigation and saves", () => {
  it("keeps the selected tab, hides global Save only there and still exposes explicit saves", async () => {
    const { elements, requests, tabs } = loadShell({ drivers: [], loadpoints: [] });
    let context;
    tabs.control = { render: ctx => { context = ctx; return ''; } };
    tabs.loadpoints = { render: ctx => { context = ctx; return ''; } };
    elements['settings-btn'].handlers.click();
    await settled();
    context.navigateTab('loadpoints');
    assert.equal(elements['settings-save'].hidden, true);
    assert.equal(elements['settings-save'].style.display, 'none');
    await context.saveConfig();
    assert.equal(requests.filter(r => r.opts?.method === 'POST').length, 1);
    context.navigateTab('control');
    assert.equal(elements['settings-save'].hidden, false);
  });
});
