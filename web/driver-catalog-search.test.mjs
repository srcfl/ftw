// Finding a driver for hardware you own.
//
// The catalog was a dropdown of 37 entries in alphabetical order, and it made
// the operator translate: you know you have an SH10RT, not that you need
// "Sungrow SH Hybrid Inverter". Four of those 37 have run on customer
// hardware; the other 33 sat mixed in among them.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

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
    verification_status: "experimental", version: "1.0.0",
  },
  {
    id: "sungrow", name: "Sungrow SH Hybrid Inverter", manufacturer: "Sungrow",
    protocols: ["modbus"], capabilities: ["meter", "pv", "battery"],
    verification_status: "production", version: "1.4.0",
    tested_models: ["SH5.0RT", "SH6.0RT", "SH8.0RT", "SH10RT"],
  },
  {
    id: "ctek-chargestorm", name: "CTEK Chargestorm (API v1)", manufacturer: "CTEK",
    protocols: ["modbus"], capabilities: ["ev"],
    verification_status: "beta", version: "0.2.0",
    tested_models: ["Chargestorm Connected 2", "Chargestorm Connected 3"],
  },
  {
    id: "ferroamp", name: "Ferroamp EnergyHub", manufacturer: "Ferroamp",
    protocols: ["mqtt"], capabilities: ["meter", "pv", "battery"],
    verification_status: "production", version: "1.0.0",
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

test("drivers proven on hardware come first", () => {
  const api = load();
  // Alphabetically Ambibox leads and the two proven drivers are buried. That
  // ordering is what made the old list actively misleading.
  assert.equal(names(api.searchCatalog(CATALOG, "")),
    "ferroamp sungrow ctek-chargestorm ambibox_v2x");
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
