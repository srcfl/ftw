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

function loadTab() {
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
    // A box that has never had app_link in its YAML — the state Fredrik's
    // box was in. Without this the checkbox writes into nothing and Save
    // posts a config with no app_link key at all.
    const config = {};
    render(config);
    // Compared field by field: the object is built inside the vm sandbox, so
    // its prototype comes from another realm and deepStrictEqual refuses it.
    assert.ok(config.app_link, "app_link was not created");
    assert.equal(config.app_link.enabled, false);
  });

  it("says a restart is needed", () => {
    // Not buried in a tooltip: this is the one thing about this setting that
    // does not behave like the rest of the page.
    assert.match(render({}), /[Rr]estart/);
  });

  it("starts with the pairing button disabled", () => {
    // It is enabled once /api/app-link/status reports the uplink running.
    // Starting enabled means the first press of a fresh page fails.
    assert.match(render({}), /id="app-link-pair"[^>]*disabled/);
  });
});
