// Finding a driver for hardware you own.
//
// The catalog was a dropdown of 37 entries in alphabetical order, and it made
// the operator translate: you know you have an SH10RT, not that you need
// "Sungrow SH Hybrid Inverter".

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

// Settings searches through device-flow.js, which the page loads first.
const source = readFileSync(new URL("./device-flow.js", import.meta.url), "utf8") + "\n" +
  readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

function load() {
  const window = { FTWSettings: { tabs: {} } };
  vm.runInNewContext(source, {
    document: { createElement: () => ({ style: {}, dataset: {}, appendChild() {}, addEventListener() {} }), getElementById: () => null },
    fetch: async () => ({ ok: true, json: async () => ({}) }),
    window,
  });
  return window.FTWSettings.driverVersions;
}

// A slice of the real catalog, including the things that made the old list
// hard to read.
const CATALOG = [
  {
    id: "ambibox_v2x", name: "Ambibox V2X", manufacturer: "Ambibox",
    protocols: ["mqtt"], capabilities: ["v2x_charger"],
    run_by: 0, version: "1.0.0",
  },
  {
    id: "sungrow", name: "Sungrow SH Hybrid Inverter", manufacturer: "Sungrow",
    protocols: ["modbus"], capabilities: ["meter", "pv", "battery"],
    run_by: 9, version: "1.4.0",
    tested_models: ["SH5.0RT", "SH6.0RT", "SH8.0RT", "SH10RT"],
  },
  {
    id: "ctek-chargestorm", name: "CTEK Chargestorm (API v1)", manufacturer: "CTEK",
    protocols: ["modbus"], capabilities: ["ev"],
    run_by: 1, version: "0.2.0",
    tested_models: ["Chargestorm Connected 2", "Chargestorm Connected 3"],
  },
  {
    id: "ferroamp", name: "Ferroamp EnergyHub", manufacturer: "Ferroamp",
    protocols: ["mqtt"], capabilities: ["meter", "pv", "battery"],
    run_by: 14, version: "1.0.0",
    tested_models: ["EnergyHub XL"],
  },
];

const names = (entries) => entries.map((e) => e.id).join(" ");

test("searching finds the driver by the model printed on the unit", () => {
  const api = load();
  // The whole point: an operator reads SH10RT off the inverter, not the
  // driver's name out of a catalog.
  assert.equal(names(api.searchCatalog(CATALOG, "SH10RT")), "sungrow");
  assert.equal(names(api.searchCatalog(CATALOG, "Chargestorm Connected 3")), "ctek-chargestorm");
  assert.equal(names(api.searchCatalog(CATALOG, "EnergyHub XL")), "ferroamp");
});

test("searching also takes the manufacturer or the driver's own name", () => {
  const api = load();
  assert.equal(names(api.searchCatalog(CATALOG, "ferroamp")), "ferroamp");
  assert.equal(names(api.searchCatalog(CATALOG, "hybrid")), "sungrow");
  assert.equal(names(api.searchCatalog(CATALOG, "mqtt")), "ferroamp ambibox_v2x");
});

test("every word has to match, so more typing narrows", () => {
  const api = load();
  assert.equal(names(api.searchCatalog(CATALOG, "sungrow hybrid")), "sungrow");
  // Two terms that never occur together find nothing rather than everything.
  assert.equal(api.searchCatalog(CATALOG, "sungrow ferroamp").length, 0);
});

test("the drivers most homes run come first", () => {
  const api = load();
  assert.equal(names(api.searchCatalog(CATALOG, "")),
    "ferroamp sungrow ctek-chargestorm ambibox_v2x");
});

test("with no counts the list is alphabetical", () => {
  const api = load();
  const bare = CATALOG.map(({ run_by, ...entry }) => entry);
  assert.equal(names(api.searchCatalog(bare, "")),
    "ambibox_v2x ctek-chargestorm ferroamp sungrow");
});

test("a driver whose name already starts with its maker is not repeated", () => {
  const api = load();
  // "CTEK Chargestorm (API v1) — CTEK" was how the old list read.
  assert.equal(api.catalogTitle(CATALOG[2]), "CTEK Chargestorm (API v1)");
  assert.equal(api.catalogTitle(CATALOG[1]), "Sungrow SH Hybrid Inverter");
  // But one that does not carry it still gets it.
  assert.equal(
    api.catalogTitle({ name: "EnergyHub", manufacturer: "Ferroamp" }),
    "Ferroamp EnergyHub");
});

test("an empty search shows everything rather than nothing", () => {
  const api = load();
  assert.equal(api.searchCatalog(CATALOG, "").length, CATALOG.length);
  assert.equal(api.searchCatalog(CATALOG, "   ").length, CATALOG.length);
});

test("searching is not case sensitive", () => {
  const api = load();
  assert.equal(names(api.searchCatalog(CATALOG, "sh10rt")), "sungrow");
  assert.equal(names(api.searchCatalog(CATALOG, "FERROAMP")), "ferroamp");
});

// Run the real Devices tab: it fetches the catalog and draws one card per
// driver. A name left behind when the search moved to device-flow.js once
// stopped this at "Loading catalog…" while the unit tests above passed.
test("Settings draws the catalog cards and narrows them as you type", async () => {
  const byId = {};
  const el = (tag) => {
    const node = {
      tag, children: [], dataset: {}, style: {}, className: "", value: "", handlers: {},
      options: [], selectedIndex: 0, text: "",
      // As in a browser, setting the text replaces the children.
      get textContent() { return node.text; },
      set textContent(v) { node.text = v; node.children = []; },
      set innerHTML(v) { node.children = []; node.options = []; },
      appendChild(child) { node.children.push(child); if (tag === "select") node.options.push(child); return child; },
      addEventListener(name, fn) { node.handlers[name] = fn; },
      dispatchEvent() {},
      querySelector() { return null; },
      insertBefore(child) { node.children.unshift(child); return child; },
    };
    return node;
  };
  for (const id of ["driver-catalog-picker", "driver-catalog-results", "driver-catalog-search"]) {
    byId[id] = el(id === "driver-catalog-picker" ? "select" : "div");
  }
  const window = { FTWSettings: { tabs: {} } };
  const sandbox = {
    window, Event: class { constructor(type) { this.type = type; } },
    document: { createElement: el, getElementById: (id) => byId[id] || null },
    fetch: async (path) => ({ ok: true, json: async () => (path === "/api/drivers/catalog" ? { entries: CATALOG } : {}) }),
  };
  sandbox.window.document = sandbox.document;
  vm.runInNewContext(source, sandbox);
  window.FTWSettings.tabs.devices.after({
    config: { drivers: [] }, escHtml: String, help: () => "",
    bodyEl: { querySelectorAll: () => [], querySelector: () => null },
  });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  const titles = () => byId["driver-catalog-results"].children.map((card) => card.children[0].textContent);
  assert.equal(titles().length, CATALOG.length);
  byId["driver-catalog-search"].value = "SH10RT";
  byId["driver-catalog-search"].handlers.input();
  assert.deepEqual(titles(), ["Sungrow SH Hybrid Inverter"]);
});
