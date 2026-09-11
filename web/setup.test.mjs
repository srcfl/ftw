// node --test web/setup.test.mjs
//
// Structural / lint-style tests for the setup wizard. setup.js is a
// DOM-coupled IIFE that runs goStep(1) on load — it can't be imported
// under `node --test` without a DOM polyfill (the repo ships none). So we
// regex over the source + the wizard HTML to lock in the Job 1 EV-setup
// bug fixes:
//   1. the id mismatch (#ev-username in HTML vs ev-email reads in JS)
//   2. the empty #ev-provider <select> that left the whole EV flow dead
//   3. buildConfig shaping the ev_charger block per provider transport
//      (easee=http username/password/serial, ctek=modbus host/port/unit)

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const JS = readFileSync(join(__dirname, "setup.js"), "utf8");
const HTML = readFileSync(join(__dirname, "setup.html"), "utf8");
const DEVICES_JS = readFileSync(join(__dirname, "settings", "tabs", "devices.js"), "utf8");
const PRICE_JS = readFileSync(join(__dirname, "settings", "tabs", "price.js"), "utf8");

describe("setup wizard EV charger — id mismatch fix (Job 1)", () => {
  it("never reads the non-existent #ev-email element", () => {
    assert.doesNotMatch(JS, /getElementById\(['"]ev-email['"]\)/,
      "the EV input is #ev-username in setup.html — ev-email reads are the confirmed bug");
  });

  it("reads #ev-username, the id that actually exists in the HTML", () => {
    assert.match(HTML, /id=["']ev-username["']/,
      "the username input must keep the id the JS reads");
    assert.match(JS, /getElementById\(['"]ev-username['"]\)/,
      "loadEVChargers + buildConfig must read #ev-username");
  });
});

describe("setup wizard EV charger — provider options (Job 1)", () => {
  it("declares the known EV providers in JS so the empty <select> gets populated", () => {
    // The HTML ships only <option value="">None</option>; JS must fill the rest.
    assert.match(JS, /EV_PROVIDERS\s*=/,
      "a provider table must drive the #ev-provider options");
    assert.match(JS, /value:\s*['"]easee['"]/,
      "Easee (the cloud HTTP provider) must be selectable");
    assert.match(JS, /value:\s*['"]zaptec['"]/,
      "Zaptec (the cloud HTTP provider) must be selectable");
    assert.match(JS, /value:\s*['"]tesla-wc['"]/,
      "Tesla Wall Connector (local HTTP) must be selectable");
    assert.match(JS, /value:\s*['"]ctek['"]/,
      "CTEK (the local Modbus provider) must be selectable");
    assert.match(JS, /populateEVProviders/,
      "a function must append the provider options into #ev-provider");
  });

  it("toggles the http vs modbus field block by provider transport", () => {
    assert.match(JS, /ev-fields-http/,
      "the HTTP credentials block must be revealed for cloud providers");
    assert.match(JS, /ev-fields-modbus/,
      "the Modbus block must be revealed for local providers");
    assert.match(JS, /ev-fields-http-local/,
      "the LAN HTTP block must be revealed for Tesla Wall Connector");
    assert.match(HTML, /id=["']ev-http-host["']/,
      "the Tesla Wall Connector host field must exist in the wizard");
  });
});

describe("setup wizard EV charger — buildConfig shapes the block per provider", () => {
  it("emits a modbus{host,...} block for Modbus providers", () => {
    assert.match(JS, /ev\.modbus\s*=\s*\{\s*host:/,
      "ctek must serialise as ev_charger.modbus.host (matches Go EVChargerModbus)");
    assert.match(JS, /unit_id/,
      "the modbus block must carry unit_id when set");
  });

  it("emits username/password/serial for HTTP providers", () => {
    assert.match(JS, /ev\.username\s*=/, "easee carries a username");
    assert.match(JS, /ev\.serial\s*=/, "easee carries the looked-up charger serial");
  });

  it("emits http.base_url for local HTTP providers", () => {
    assert.match(JS, /http-local/,
      "Tesla Wall Connector uses the http-local transport");
    assert.match(JS, /ev\.http\s*=\s*\{\s*base_url:/,
      "tesla-wc must serialise as ev_charger.http.base_url");
  });

  it("does not regress to hard-coded 'Easee' in the review summary", () => {
    assert.match(JS, /evProviderLabel\(/,
      "the review screen must label the actual chosen provider, not always Easee");
  });
});

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

describe("setup wizard — fingerprinted network scan", () => {
  it("requests fingerprint enrichment and renders the best match", () => {
    assert.match(JS, /\/api\/scan\?fingerprint=1/);
    assert.match(JS, /d\.matches\[0\]/,
      "the highest-confidence API match should be shown first");
    assert.match(HTML, /Detected device/);
  });

  it("preselects the matched catalog driver for normal configuration", () => {
    assert.match(JS, /selectedDevice\.matchedFilename/);
    assert.match(JS, /entry\.filename === selectedDevice\.matchedFilename/);
  });
});

describe("setup wizard — read-only battery gateways", () => {
  it("never turns a read-only gateway into a controllable battery", () => {
    assert.match(JS, /!selectedCatalog\.read_only/,
      "battery capacity must be hidden and ignored for a read-only catalog entry");
    assert.match(JS, /driver\.battery_telemetry_only\s*=\s*true/,
      "read-only gateways still need their physical battery telemetry admitted");
  });

  it("honours a driver's local-API port instead of the generic HTTP default", () => {
    assert.match(JS, /selectedCatalog\.connection_defaults\.port/,
      "Sourceful Zap declares port 80 and setup must retain that default");
  });

  it("applies the same telemetry-only safety in Settings", () => {
    assert.match(DEVICES_JS, /entry\.read_only\s*&&\s*entryCaps\.indexOf\("battery"\)/);
    assert.match(DEVICES_JS, /driver\.battery_telemetry_only\s*=\s*true/);
    assert.match(DEVICES_JS, /telemetry only/,
      "operators should see why Zap has no battery-capacity control field");
  });

  it("treats an EV-only local HTTP driver as a host field, not a cloud account", () => {
    assert.match(DEVICES_JS, /entryCaps\.indexOf\("ev"\) >= 0 && hosts\.length === 0/,
      "Tesla Wall Connector has ev + no catalog http_hosts and must get config.host");
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

describe("setup wizard driver picker — not-listed escape hatch (#757)", () => {
  it("appends the not-listed option after the catalog entries", () => {
    assert.match(JS, /NOT_LISTED\s*=\s*['"]__not_listed__['"]/,
      "the sentinel must be a value that can never parse as a catalog index");
    assert.match(JS, /My device is not listed/,
      "the picker must offer the escape hatch in its own words");
  });

  it("handles the sentinel before parsing a catalog index", () => {
    assert.match(JS, /sel\.value\s*===\s*NOT_LISTED[\s\S]*?parseInt\(sel\.value,\s*10\)/,
      "onDriverSelected must branch on the sentinel before parseInt runs on it");
  });

  it("ships the guidance panel with both forward paths", () => {
    assert.match(HTML, /id=["']driver-not-listed["']/,
      "the panel the sentinel reveals must exist in the markup");
    assert.match(HTML, /Settings\s*&rarr;\s*Devices/,
      "it must say where repository drivers install after setup");
    assert.match(HTML, /device-drivers\/issues/,
      "it must link to requesting a driver that does not exist yet");
    assert.match(HTML, /skipUnlistedDevice\(\)/,
      "it must let onboarding continue without the device");
  });

  it("skips to the devices summary only when a device already exists", () => {
    assert.match(JS, /skipUnlistedDevice[\s\S]*?configuredDrivers\.length > 0 \? 6 : 7/,
      "an empty summary step is a second dead-end; go straight to integrations");
  });
});

describe("setup wizard — mDNS-first device addressing", () => {
  it("carries the discovered hostname into the selected device", () => {
    assert.match(JS, /hostname:\s*dev\.hostname/,
      "useScanDevice must not drop the scan's mDNS/DNS hostname");
  });

  it("prefers a self-broadcast .local name over the raw IP when prefilling", () => {
    assert.match(JS, /isMDNSName\(selectedDevice\.hostname\)\s*\?\s*selectedDevice\.hostname\s*:\s*selectedDevice\.ip/,
      "the host field must prefill the mDNS name when the device broadcasts one");
    assert.match(JS, /\\\.local\\\.\?\$/,
      "only RFC 6762 .local names qualify — other DNS names depend on the router");
  });

  it("tells the operator to reserve a raw IP in the router's DHCP pool", () => {
    assert.match(HTML, /id=["']drv-host-hint["']/,
      "the addressing hint element must exist under the host field");
    assert.match(JS, /Reserve it for the device in your router/,
      "an IP-literal host must surface the DHCP-reservation warning");
    assert.match(JS, /addEventListener\(['"]input['"],\s*updateHostHint\)/,
      "the hint must track manual edits of the host field");
  });
});
