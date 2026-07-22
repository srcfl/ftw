import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const devices = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

test("Update Center lists configured drivers instead of the full repository", () => {
  assert.match(badge, /apiFetch\("\/api\/drivers\/catalog"\)/);
  assert.match(badge, /apiFetch\("\/api\/config"\)/);
  assert.match(badge, /device_repository\/catalog\?channel=beta/);
  assert.match(badge, /configured\.has\(driverFileKey/);
  assert.match(badge, /Installed drivers · signed updates/);
  assert.doesNotMatch(badge, /apiFetch\("\/api\/device_repository\/catalog"\)/);
  assert.doesNotMatch(badge, /No managed driver candidates cached yet/);
});

test("Update Center can install one signed beta driver without a Core update", () => {
  assert.match(badge, /Try beta /);
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

test("Update Center only offers an action for a newer signed version", () => {
  assert.match(badge, /entry\.update_available && entry\.repository_id && entry\.upstream_version/);
  assert.match(badge, /Update to /);
  assert.doesNotMatch(badge, /entry\.update_available \|\| !entry\.installed/);
  assert.doesNotMatch(badge, /\? "Update" : "Install"/);
});

test("Devices links to repository support data without traffic-light claims", () => {
  assert.match(devices, /device-drivers\/blob\/main\/SUPPORT_STATUS\.md/);
  assert.doesNotMatch(devices, /production — verified on real hardware/);
  assert.doesNotMatch(devices, /awaiting a second/);
  assert.doesNotMatch(devices, /ported from reference/);
  assert.doesNotMatch(devices, /[🟢🟡🔴]/u);
});
