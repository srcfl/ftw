import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const devices = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

test("the dialog lists every configured driver, not just the updatable ones", () => {
  assert.match(badge, /apiFetch\("\/api\/drivers\/catalog"\)/);
  assert.match(badge, /apiFetch\("\/api\/config"\)/);
  assert.match(badge, /device_repository\/catalog\?channel=beta/);
  assert.match(badge, /configured\.has\(driverFileKey/);
  assert.match(badge, /pending_update: stableAvailable \|\| betaAvailable/);
  // A driver with nothing waiting still gets a row, so the inventory does
  // not change shape depending on what happens to be releasable today.
  assert.doesNotMatch(badge, /return stableAvailable \|\| betaAvailable;/);
  assert.doesNotMatch(badge, /if \(entry\.source === "local"\) return false;/);
  assert.doesNotMatch(badge, /No configured drivers found/);
  assert.doesNotMatch(badge, /apiFetch\("\/api\/device_repository\/catalog"\)/);
  assert.doesNotMatch(badge, /No managed driver candidates cached yet/);
});

test("a locally edited driver is listed but offered no signed action", () => {
  assert.match(badge, /const managed = entry\.source !== "local"/);
  assert.match(badge, /title="Edited on this device; no signed version to switch to">local copy</);
  assert.match(badge, /managed && entry\.update_available/);
  assert.match(badge, /managed && betaDriver && betaDriver\.version/);
});

test("Update Center can install one signed beta driver without a Core update", () => {
  assert.match(badge, /"Beta " \+ escapeHTML\(betaDriver\.version\)/);
  assert.match(badge, /data-channel="beta"/);
  assert.match(badge, /channel \? \{ channel \} : \{\}/);
  assert.match(badge, /Only affected driver instances restart/);
});

test("Devices can add one driver straight from the signed beta channel", () => {
  assert.match(devices, /id="driver-catalog-channel"/);
  assert.match(devices, /Beta · test one driver/);
  assert.match(devices, /device_repository\/catalog\?channel=beta/);
  assert.match(devices, /JSON\.stringify\(\{channel: "beta", version: chosen\.dataset\.version\}\)/);
  assert.match(devices, /Beta installs only the selected signed driver/);
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

test("Update Center only offers stable or beta when that signed version differs", () => {
  assert.match(badge, /entry\.update_available && entry\.repository_id && entry\.upstream_version/);
  assert.match(badge, /"Stable " \+ escapeHTML\(entry\.upstream_version\)/);
  assert.match(badge, /betaDriver\.version !== current/);
  assert.doesNotMatch(badge, />current<\/span>/);
  assert.doesNotMatch(badge, /entry\.update_available \|\| !entry\.installed/);
  assert.doesNotMatch(badge, /\? "Update" : "Install"/);
});

test("the badge counts only drivers with work waiting, not the whole inventory", () => {
  assert.match(badge, /_pendingUpdates\(\)/);
  assert.match(badge, /filter\(\(entry\) => entry\.pending_update\)\.length/);
  assert.match(badge, /showDot = pending\.total > 0/);
  // The old signal lit the dot for any listed driver, which now means all
  // of them.
  assert.doesNotMatch(badge, /this\._driverCatalog\.entries\.length > 0/);
});

test("Devices links to repository support data without traffic-light claims", () => {
  assert.match(devices, /device-drivers\/blob\/main\/SUPPORT_STATUS\.md/);
  assert.doesNotMatch(devices, /production — verified on real hardware/);
  assert.doesNotMatch(devices, /awaiting a second/);
  assert.doesNotMatch(devices, /ported from reference/);
  assert.doesNotMatch(devices, /[🟢🟡🔴]/u);
});
