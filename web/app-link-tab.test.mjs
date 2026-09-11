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

  it("states both the encrypted content and visible connection metadata", () => {
    const html = render({});
    assert.match(html, /end-to-end encrypted/);
    assert.match(html, /IP/);
    assert.match(html, /when the box is connected/);
    assert.match(html, /cannot read/);
  });

  it("says this is not the Sourceful app, and LAN use needs no pairing", () => {
    const html = render({});
    assert.match(html, /not the Sourceful/);
    assert.match(html, /app\.ftw\.energy/);
    assert.match(html, /no pairing needed/);
  });

  it("starts with the pairing button disabled", () => {
    // It is enabled once /api/app-link/status reports the uplink running.
    // Starting enabled means the first press of a fresh page fails.
    assert.match(render({}), /id="app-link-pair"[^>]*disabled/);
  });

  it("keeps fleet statistics with the app, but independent of its switch", () => {
    const appWithFleet = loadTab(true);
    const config = { app_link: { enabled: false } };
    const html = appWithFleet.render({ config });

    assert.match(html, /<legend>Fleet statistics<\/legend>/);
    assert.match(html, /data-checkbox-path="fleet_ping\.enabled"/);
    assert.equal(config.fleet_ping.enabled, true, "fleet sharing should still default on");
    assert.doesNotMatch(html, /id="app-link-enabled"[^>]*checked/);
    assert.match(html, /id="fleet-ping-enabled"[^>]*checked/);
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
    assert.deepEqual(asked, ["/api/fleet-ping"]);
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
