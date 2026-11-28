import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The app link is connected at startup, so saving the checkbox is not the
// same as the box doing it. Between Save and the restart the two disagree,
// and that is exactly when someone presses "Show pairing code" and gets
// nothing back. The tab has to say which of the two states it is in.
//
// app.js is a plain IIFE, so it loads under a small shim and these tests
// drive the real render() and read the markup it produces.

const source = readFileSync(new URL("./settings/tabs/app.js", import.meta.url), "utf8");
const fleetSource = readFileSync(new URL("./settings/tabs/fleet.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");

function loadTab(withFleet = false) {
  const win = { FTWSettings: { tabs: {} } };
  const sandbox = {
    window: win,
    // render() defers its wiring; the tests read the returned markup, so the
    // callback never needs to run.
    setTimeout: () => 0,
    document: { getElementById: () => null },
    fetch: () => new Promise(() => {}),
    console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  if (withFleet) vm.runInContext(fleetSource, sandbox);
  const tab = win.FTWSettings.tabs.app;
  assert.ok(tab && typeof tab.render === "function", "app.js registered no tab");
  return tab;
}

const tab = loadTab();

function render(config) {
  return tab.render({ config });
}

describe("the app tab", () => {
  it("offers a checkbox bound to app_link.enabled", () => {
    const html = render({});
    assert.match(html, /data-checkbox-path="app_link\.enabled"/);
    assert.match(html, /type="checkbox"/);
  });

  it("reflects the saved setting", () => {
    assert.doesNotMatch(render({ app_link: { enabled: false } }), /checkbox"[^>]*checked/);
    assert.match(render({ app_link: { enabled: true } }), /checked/);
  });

  it("creates the config section when it is missing entirely", () => {
    // Load normally supplies the default. Keep the tab safe for partial config
    // fixtures and old API responses too, or the checkbox writes into nothing.
    const config = {};
    render(config);
    // Compared field by field: the object is built inside the vm sandbox, so
    // its prototype comes from another realm and deepStrictEqual refuses it.
    assert.ok(config.app_link, "app_link was not created");
    assert.equal(config.app_link.enabled, true);
  });

  it("says a restart is needed", () => {
    // Not buried in a tooltip: this is the one thing about this setting that
    // does not behave like the rest of the page.
    assert.match(render({}), /[Rr]estart/);
  });

  // The switch and its restart are for turning the app link on. A box where
  // it is saved on starts with them hidden; the status reveals them if the
  // link turns out not to be running.
  it("hides the switch and restart while the setting is on", () => {
    assert.match(render({ app_link: { enabled: true } }), /id="app-link-switch" hidden>/);
    assert.match(render({ app_link: { enabled: false } }), /id="app-link-switch">/);
  });

  // The app reaches the box through FTW Cloud, and Cloud keeps a copy
  // Sourceful can read. The page once said Sourceful could read nothing,
  // which stopped being true when pairing began to accept the Cloud terms.
  it("says pairing accepts the Cloud terms and what Cloud keeps", () => {
    const html = render({});
    assert.match(html, /id="app-link-terms" class="hint" hidden>/);
    assert.match(html, /through FTW Cloud/);
    assert.match(html, /readable copy of your energy data/);
    assert.match(html, /Pairing accepts its <a href="https:\/\/ftw\.sourceful\.energy\/privacy"/);
    assert.match(html, /The box runs without it/);
    assert.doesNotMatch(html, /cannot read/);
  });

  // The app needs pairing at home too; only a browser on the home Wi-Fi does
  // not. The page used to put the two side by side so the app sounded free
  // of pairing at home.
  it("says this is not the Sourceful app, and the app needs pairing at home too", () => {
    const html = render({});
    assert.match(html, /not the Sourceful/);
    assert.match(html, /needs pairing, at home and away/);
    assert.match(html, /browser on your home Wi-Fi needs none/);
    assert.doesNotMatch(html, /when you are away/);
  });

  it("puts the Cloud panel after the pairing, hidden until Cloud is in use", () => {
    const html = render({});
    const pairing = html.indexOf('id="app-link-slot"');
    const cloud = html.indexOf('<div id="app-cloud-panel" hidden><div data-project-statistics-settings>');
    assert.ok(pairing >= 0 && cloud > pairing, "the Cloud panel can push a code out of view");
  });

  it("starts with the pairing button disabled", () => {
    // It is enabled once /api/app-link/status reports the uplink running.
    // Starting enabled means the first press of a fresh page fails.
    assert.match(render({}), /id="app-link-pair"[^>]*disabled/);
  });

  it("keeps statistics with the app, but independent of its switch", () => {
    const appWithFleet = loadTab(true);
    const config = { app_link: { enabled: false } };
    const html = appWithFleet.render({ config });

    assert.match(html, /<legend>Anonymous statistics<\/legend>/);
    assert.match(html, /id="statistics-share"/);
    assert.doesNotMatch(html, /data-checkbox-path="fleet_ping\.enabled"/);
    assert.equal(config.fleet_ping, undefined, "statistics are not a config field");
    assert.doesNotMatch(html, /id="app-link-enabled"[^>]*checked/);
  });

  it("refreshes the fleet payload after saving from the app page", () => {
    const win = { FTWSettings: { tabs: {} } };
    const asked = [];
    const sandbox = {
      window: win,
      setTimeout: () => 0,
      document: { getElementById: () => ({ textContent: "", appendChild() {} }) },
      fetch: (path) => { asked.push(path); return new Promise(() => {}); },
      console,
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(source, sandbox);
    vm.runInContext(fleetSource, sandbox);

    win.FTWSettings.tabs.app.afterSave();
    // The app tab reads /api/cloud too, for the terms line and the panel.
    assert.deepEqual([...asked].sort(), ["/api/app-link/status", "/api/cloud", "/api/cloud", "/api/cloud/stats-policy", "/api/fleet-ping"]);
  });

  it("does not leave a standalone Fleet ping destination behind", () => {
    assert.doesNotMatch(index, /data-tab="fleet"/);
    assert.match(index, /settings\/tabs\/fleet\.js/);
  });
});

// The slot receives the answer to the pairing buttons — QR, spoken code, or
// the #951 refusal. It once rendered after the hint paragraphs, ~300 px below
// the buttons and usually under the modal's fold, so the operator pressed and
// saw nothing (field report 2026-08-29). The answer belongs where the press
// happened.
describe("the pairing-code slot", () => {
  it("renders directly under the pairing buttons, before the hints", () => {
    const html = render({});
    const actions = html.indexOf('class="app-link-actions"');
    const slot = html.indexOf('id="app-link-slot"');
    const scanHint = html.indexOf("Scan the code with the FTW app");
    assert.ok(actions >= 0 && slot >= 0 && scanHint >= 0, "expected markup missing");
    assert.ok(actions < slot, "slot must come after the buttons that fill it");
    assert.ok(slot < scanHint, "slot must come before the hint paragraphs, not after them");
  });

  it("marks a refusal as an error, not a hint", () => {
    // requestCode's catch styles the message; the class carries the red. A
    // rename here silently turns refusals back into invisible help text.
    assert.match(source, /app-link-error/);
  });
});
