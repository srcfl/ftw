// node --test web/setup.test.mjs
//
// Structural / lint-style tests for the setup wizard. setup.js is a
// DOM-coupled IIFE that runs goStep(1) on load — it can't be imported
// under `node --test` without a DOM polyfill (the repo ships none, see
// web/components/ftw-pair-card.test.mjs for the same constraint). So we
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
