// One place for driver versions: the device's Versions panel under Settings ›
// Devices. Update Center and System only point there, and adding a device
// chooses a driver type, not a version.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const devices = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");
const flowSource = readFileSync(new URL("./device-flow.js", import.meta.url), "utf8");

// The shared catalog and new-device code that Settings and the setup guide
// both run.
function deviceFlow() {
  const el = () => ({ children: [], className: "", textContent: "", appendChild(c) { this.children.push(c); return c; }, addEventListener() {} });
  const window = {};
  vm.runInNewContext(flowSource, { window, document: { createElement: el } });
  return window.FTWDeviceFlow;
}
const tagsOf = (card) => card.children.find((c) => c.className === "drv-catalog-tags").children.map((t) => t.textContent);
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");

test("the version dialog lists no driver versions and offers no driver actions", () => {
  assert.doesNotMatch(badge, /device_repository/);
  assert.doesNotMatch(badge, /driver-change|driver-versions|_driverCatalog|_refreshDriverCatalog/);
});

test("the badge counts only Core, so a new driver does not light it up", () => {
  assert.match(badge, /return \{ core, total: core \? 1 : 0 \};/);
  assert.match(badge, /showDot = pending\.total > 0/);
});

test("System points to Devices instead of refreshing driver catalogs itself", () => {
  assert.doesNotMatch(system, /sys-refresh-drivers|device_repository\/refresh/);
  assert.match(system, /versions under Devices/);
});

test("a device card announces no update; Versions is the way in", () => {
  assert.doesNotMatch(devices, /drv-module-update|Update to v/);
  assert.match(devices, /class="btn-add drv-module-versions"/);
});

test("adding a device lists channel driver types in the same list, marked by origin", () => {
  assert.doesNotMatch(devices, /driver-catalog-channel/);
  assert.match(devices, /id="driver-catalog-more"/);
  // Both signed channels: stable has drivers the release does not carry.
  assert.match(devices, /fetchCatalog\("\/api\/device_repository\/catalog"\)/);
  assert.match(devices, /fetchCatalog\("\/api\/device_repository\/catalog\?channel=beta"\)/);
  // One channel being unreachable does not hide what the other lists.
  assert.match(devices, /Promise\.allSettled\(\[/);
  const flow = deviceFlow();
  assert.deepEqual(tagsOf(flow.card({ id: "a", channel: "beta" }, { hideCapabilities: true })), ["beta"]);
  assert.deepEqual(tagsOf(flow.card({ id: "b", channel: "stable" }, { hideCapabilities: true })), ["from the driver channel"]);
  assert.deepEqual(tagsOf(flow.card({ id: "c" }, { hideCapabilities: true })), []);
  // The release's own drivers are added as they are; a channel driver is
  // fetched from its channel when the device is added.
  assert.match(devices, /populateCatalogPicker\(entries, "release"\)/);
  assert.match(devices, /\? \{channel: "beta", version: chosen\.dataset\.version\}/);
  assert.match(devices, /: \{repository_id: chosen\.dataset\.repositoryId, version: chosen\.dataset\.version\}/);
});

test("Devices configure the GoodWe register profile without editing YAML", () => {
  assert.match(devices, /id="driver-catalog-profile"/);
  assert.match(devices, /data-path="drivers\.' \+ dIdx \+ '\.config\.profile"/);
  const flow = deviceFlow();
  const goodwe = { id: "goodwe", version: "1.0.2", path: "drivers/goodwe.lua", protocols: ["modbus"] };
  const plain = (v) => JSON.parse(JSON.stringify(v));
  assert.deepEqual(plain(flow.configProfiles(goodwe).map((p) => p.value)), ["community-v1", "gw8kn-et-hk3000"]);
  assert.equal(flow.configProfiles({ id: "goodwe", version: "1.0.1" }).length, 0, "an older driver gets no profile");
  const driver = flow.newDriver(goodwe, { name: "gw", profile: "gw8kn-et-hk3000" });
  assert.deepEqual(plain(driver.config), { profile: "gw8kn-et-hk3000" });
  assert.equal(driver.capabilities.modbus.unit_id, 247);
});

test("Devices links to repository support data without traffic-light claims", () => {
  assert.match(devices, /device-drivers\/blob\/main\/SUPPORT_STATUS\.md/);
  assert.doesNotMatch(devices, /production — verified on real hardware/);
  assert.doesNotMatch(devices, /awaiting a second/);
  assert.doesNotMatch(devices, /ported from reference/);
  assert.doesNotMatch(devices, /[🟢🟡🔴]/u);
});
