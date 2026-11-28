// node --test web/setup.test.mjs
//
// What the guide and Settings share when a device is added. The guide's own
// behaviour runs in setup-connection.test.mjs against the real page; these
// run the shared new-device code in device-flow.js and check the few
// promises that live in the markup.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const __dirname = dirname(fileURLToPath(import.meta.url));
const JS = readFileSync(join(__dirname, "setup.js"), "utf8");
const HTML = readFileSync(join(__dirname, "setup.html"), "utf8");
const DEVICES_JS = readFileSync(join(__dirname, "settings", "tabs", "devices.js"), "utf8");
const PRICE_JS = readFileSync(join(__dirname, "settings", "tabs", "price.js"), "utf8");
const FLOW_JS = readFileSync(join(__dirname, "device-flow.js"), "utf8");

function deviceFlow() {
  const window = {};
  vm.runInNewContext(FLOW_JS, { window, document: { createElement: () => ({}) } });
  return window.FTWDeviceFlow;
}
const plain = (v) => JSON.parse(JSON.stringify(v));

describe("setup wizard — ?step=N deep-link (Job 4)", () => {
  it("reads the step param from the URL on init", () => {
    assert.match(JS, /URLSearchParams\(window\.location\.search\)/,
      "init must parse ?step=N from the query string");
    assert.match(JS, /\.get\(['"]step['"]\)/);
  });

  it("clamps the step into the valid 1..TOTAL_STEPS range", () => {
    // Out-of-range or junk params must not goStep() to a non-existent step.
    assert.match(JS, /n\s*>\s*TOTAL_STEPS/,
      "the upper bound must clamp to TOTAL_STEPS");
    assert.match(JS, /goStep\(initialStep\(\)\)/,
      "init must drive goStep with the clamped value, not a hard-coded 1");
  });
});

describe("new devices — the driver block Settings and the guide both add", () => {
  it("never turns a read-only gateway into a controllable battery", () => {
    const flow = deviceFlow();
    const zap = { id: "zap", filename: "zap.lua", path: "drivers/zap.lua", protocols: ["http"],
      capabilities: ["meter", "pv", "battery"], read_only: true, connection_defaults: { host: "zap.local", port: 80 } };
    const driver = plain(flow.newDriver(zap, { name: "zap" }));
    assert.equal(driver.battery_telemetry_only, true, "the battery reading is admitted, not dispatched");
    assert.equal(driver.battery_capacity_wh, undefined);
    assert.deepEqual(driver.config, { host: "zap.local", read_pv: false, read_battery: false });
  });

  it("admits a read-only hybrid's battery reading too, whatever its protocol", () => {
    const flow = deviceFlow();
    const huawei = { id: "huawei", path: "drivers/huawei.lua", protocols: ["modbus"],
      capabilities: ["meter", "pv", "battery"], read_only: true };
    assert.equal(flow.newDriver(huawei).battery_telemetry_only, true);
    const sungrow = { id: "sungrow", path: "drivers/sungrow.lua", protocols: ["modbus"], capabilities: ["meter", "pv", "battery"] };
    assert.equal(flow.newDriver(sungrow).battery_telemetry_only, undefined, "a driver that controls stays in the pool");
  });

  it("gives each kind of device the fields it needs", () => {
    const flow = deviceFlow();
    const kind = (entry) => flow.addressKind(flow.newDriver(entry));
    assert.equal(kind({ protocols: ["modbus"] }), "modbus");
    assert.equal(kind({ protocols: ["mqtt"] }), "mqtt");
    // Tesla Wall Connector: ev, local, no catalog http_hosts.
    assert.equal(kind({ protocols: ["http"], capabilities: ["ev"] }), "host");
    assert.equal(kind({ protocols: ["http"], capabilities: ["ev"], http_hosts: ["api.easee.com"] }), "account");
    assert.equal(kind({ protocols: ["http"], capabilities: ["vehicle"] }), "", "a car is set up in Settings");
    assert.equal(kind({ protocols: ["http"], capabilities: ["apicreds"] }), "", "an OAuth sign-in is set up in Settings");
  });

  it("takes the port and Unit ID from the driver unless the owner sets them", () => {
    const flow = deviceFlow();
    const sma = { id: "sma_pv", path: "drivers/sma_pv.lua", protocols: ["modbus"], connection_defaults: { port: 502, unit_id: 3 } };
    const driver = flow.setAddress(flow.newDriver(sma), sma, { host: "192.168.1.9" });
    assert.deepEqual(plain(driver.capabilities.modbus), { host: "192.168.1.9", port: 502, unit_id: 3 });
    flow.setAddress(driver, sma, { host: "192.168.1.9", port: "1502", unitId: "1" });
    assert.deepEqual(plain(driver.capabilities.modbus), { host: "192.168.1.9", port: 1502, unit_id: 1 });
  });

  it("says reads only for a read-only driver with a battery or solar, never a guess", () => {
    const flow = deviceFlow();
    assert.equal(flow.readsOnly({ read_only: true, capabilities: ["meter", "pv", "battery"] }), true, "Huawei");
    assert.equal(flow.readsOnly({ read_only: true, capabilities: ["pv"] }), true);
    assert.equal(flow.readsOnly({ read_only: true, capabilities: ["meter"] }), false, "a meter is only ever read");
    assert.equal(flow.readsOnly({ capabilities: ["meter", "pv", "battery"] }), false, "unstated is unknown");
  });

  it("Settings adds devices through the same code", () => {
    assert.match(DEVICES_JS, /flow\(\)\.newDriver\(entry, \{ name: name, profile:/);
    assert.match(DEVICES_JS, /flow\(\)\.test\(driver, apiFetch\)/);
    assert.match(DEVICES_JS, />reads only</,
      "operators should see why Zap has no battery-capacity control field");
  });

  it("lets a gateway battery source be disabled when a native driver owns the same battery", () => {
    assert.match(DEVICES_JS, /class="drv-disable-battery"/);
    assert.match(DEVICES_JS, /drivers\.' \+ idx \+ '\.config\.disable_battery/);
    assert.match(DEVICES_JS, /caps\.indexOf\("meter"\) >= 0 && caps\.indexOf\("battery"\) >= 0/);
    assert.match(DEVICES_JS, /prevents Combined from counting its power twice/);
  });

  it("tells the operator that Zap is the P1/HAN meter by default", () => {
    assert.match(JS, /P1\/HAN site meter by default/,
      "setup must say Zap is the meter unless the operator opts in");
    assert.match(DEVICES_JS, /class="zap-p1-note"/);
    assert.match(DEVICES_JS, /Zap never writes/);
  });

  it("offers opt-in Zap PV and battery reads in Devices", () => {
    assert.match(DEVICES_JS, /class="drv-read-pv"/);
    assert.match(DEVICES_JS, /drivers\.' \+ idx \+ '\.config\.read_pv/);
    assert.match(DEVICES_JS, /class="drv-read-battery"/);
    assert.match(DEVICES_JS, /drivers\.' \+ idx \+ '\.config\.read_battery/);
    assert.match(DEVICES_JS, /Read PV from devices on this Zap/);
  });

  it("saves an unchecked Zap read as false instead of leaving the key out", () => {
    // Core counts a missing read_pv as PV that may report and waits for it.
    assert.match(DEVICES_JS, /if \(d\.config\.read_pv == null\) d\.config\.read_pv = false;/);
    assert.match(DEVICES_JS, /if \(d\.config\.read_battery == null\) d\.config\.read_battery = false;/);
  });
});

describe("setup wizard — devices step", () => {
  it("scans with fingerprints and offers the device list Settings uses", () => {
    assert.match(JS, /\/api\/scan\?fingerprint=1/);
    assert.match(HTML, /id="catalog-search"/);
    assert.match(HTML, /<script src="\/device-flow\.js[^"]*"><\/script>\s*<script src="\/setup\.js/,
      "the shared flow loads before the guide");
  });

  it("lets the owner ask for a missing driver and go on without it", () => {
    assert.match(HTML, /device-drivers\/issues/);
    assert.match(HTML, /continue without it/);
    assert.match(JS, /Continue without devices/);
  });
});

describe("price provider defaults", () => {
  it("offers Sourceful first and selects it for new setup configs", () => {
    assert.match(HTML, /<select id=["']price-provider["']>\s*<option value=["']sourceful["']>/,
      "the first option is the browser-selected default");
  });

  it("uses Sourceful as the Settings default while retaining alternatives", () => {
    assert.match(PRICE_JS, /\["sourceful", "elprisetjustnu", "entsoe", "none"\], "sourceful"/);
  });
});

describe("setup wizard — the house password on a new box", () => {
  it("asks for it on the last step and sends it with the settings", () => {
    assert.match(HTML, /id=["']house-password-step["'][^>]*hidden/,
      "the step starts hidden until the wizard knows it claims a new box");
    assert.match(HTML, /id=["']house-password["']/);
    assert.match(HTML, /id=["']house-password-confirm["']/);
    assert.match(JS, /cfg\.house_password\s*=\s*password/,
      "the wizard sends the password with the settings it saves");
  });

  it("asks only on a new box, never through Home Assistant", () => {
    assert.match(JS, /firstRun\s*=\s*r\.status\s*===\s*404/,
      "a box that already runs answers /api/auth/status and keeps its password");
    assert.match(JS, /firstRun\s*&&\s*!window\.FTWHomeAssistant/);
  });

  it("refuses only an empty or mistyped password; a short one gets a warning", () => {
    assert.match(JS, /!password\s*\|\|\s*password\s*!==\s*document\.getElementById\(['"]house-password-confirm['"]\)\.value/);
    assert.doesNotMatch(JS + HTML, /at least 10|minlength="10"|length\s*<\s*10/);
    assert.match(JS, /Short passwords are easy to guess/);
  });
});

describe("setup wizard — household clock", () => {
  it("stores the browser's zone as site.time_zone", () => {
    assert.match(JS, /time_zone:\s*browserTimeZone\(\)/,
      "a Docker box runs on UTC; the home's zone comes from the browser that sets it up");
    assert.match(JS, /resolvedOptions\(\)\.timeZone/);
  });

  it("names the clock on the review screen", () => {
    assert.match(JS, /' · clock ' \+ esc\(clock\)/);
  });
});
