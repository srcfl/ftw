// One place for driver versions: the device's Versions panel under Settings ›
// Devices. Update Center and System only point there, and adding a device
// chooses a driver type, not a version.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const devices = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");

test("Update Center lists no driver versions and offers no driver actions", () => {
  assert.doesNotMatch(badge, /device_repository/);
  assert.doesNotMatch(badge, /driver-change|driver-versions|_driverCatalog|_refreshDriverCatalog/);
  assert.match(badge, /Versions are chosen per device under Settings › Devices\./);
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
  assert.match(devices, /e\.channel === "beta" \? "beta" : "from the driver channel"/);
  // The release's own drivers are added as they are; a channel driver is
  // fetched from its channel when the device is added.
  assert.match(devices, /populateCatalogPicker\(entries, "release"\)/);
  assert.match(devices, /\? \{channel: "beta", version: chosen\.dataset\.version\}/);
  assert.match(devices, /: \{repository_id: chosen\.dataset\.repositoryId, version: chosen\.dataset\.version\}/);
});

test("Devices configure the GoodWe register profile without editing YAML", () => {
  assert.match(devices, /DRIVER_CONFIG_PROFILES\s*=\s*\{/);
  assert.match(devices, /goodwe:\s*\[/);
  assert.match(devices, /value: "community-v1"/);
  assert.match(devices, /value: "gw8kn-et-hk3000"/);
  assert.match(devices, /id="driver-catalog-profile"/);
  assert.match(devices, /data-path="drivers\.' \+ dIdx \+ '\.config\.profile"/);
  assert.match(devices, /driver\.config = \{ profile: selectedProfile\.value \}/);
  assert.match(devices, /unit_id = selectedProfile\.unitId/);
});

test("Devices links to repository support data without traffic-light claims", () => {
  assert.match(devices, /device-drivers\/blob\/main\/SUPPORT_STATUS\.md/);
  assert.doesNotMatch(devices, /production — verified on real hardware/);
  assert.doesNotMatch(devices, /awaiting a second/);
  assert.doesNotMatch(devices, /ported from reference/);
  assert.doesNotMatch(devices, /[🟢🟡🔴]/u);
});
