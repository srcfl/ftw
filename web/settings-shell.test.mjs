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

function stubElement() {
  return {
    textContent: "",
    className: "",
    innerHTML: "",
    dataset: {},
    style: {},
    handlers: {},
    classList: { add() {}, remove() {}, toggle() {} },
    addEventListener(type, fn) { this.handlers[type] = fn; },
    querySelectorAll: () => [],
    appendChild() {},
  };
}

function loadShell(saveResponse, ok = true) {
  const elements = {};
  for (const id of ELEMENT_IDS) elements[id] = stubElement();

  const requests = [];
  const responses = {};
  const sandbox = {
    window: { FTWSettings: { tabs: {} } },
    document: {
      getElementById: (id) => elements[id] || null,
      createElement: () => stubElement(),
    },
    fetch(path, opts) {
      requests.push({ path, opts });
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
  return { elements, requests, responses, tabs: sandbox.window.FTWSettings.tabs,
    loadTab: file => vm.runInContext(readFileSync(new URL(file, import.meta.url), "utf8"), sandbox) };
}

// One turn of the event loop, which is all the save chain needs to settle.
const settled = () => new Promise((resolve) => setImmediate(resolve));

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
