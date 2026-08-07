// Turning a driver's write path on, from the settings screen.
//
// Every driver in the catalog reads. One of them can also write — the NIBE
// S-series solar surplus feed — and turning it on meant hand-editing two
// separate keys in config.yaml on the gateway, one of them a host capability
// most owners never see. An owner who can install the driver from a card in
// the UI could not then use the one thing it was built for.
//
// What these tests hold in place is not the fieldset but its rules: it exists
// only where the driver itself declares the write path, both gates move
// together, and neither moves at all without a ceiling on what can be sent.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

function load() {
  const window = { FTWSettings: { tabs: {} } };
  vm.runInNewContext(source, {
    document: {
      createElement: () => ({ style: {}, dataset: {}, appendChild() {}, addEventListener() {} }),
      getElementById: () => null,
    },
    fetch: async () => ({ ok: true, json: async () => ({}) }),
    window,
  });
  return window.FTWSettings.writeFeed;
}

// Just enough of the DOM to run the fieldset the tab writes into a slot:
// the inputs it puts there, and the listeners it hangs off them.
function parseNodes(html) {
  const nodes = [];
  for (const [, tag, rawAttrs] of html.matchAll(/<(input|p)\b([^>]*)>/g)) {
    const attrs = {};
    for (const [, name, value] of rawAttrs.matchAll(/([a-z-]+)(?:="([^"]*)")?/g)) {
      attrs[name] = value === undefined ? "" : value;
    }
    const listeners = new Map();
    nodes.push({
      tag,
      className: attrs.class || "",
      type: attrs.type || "",
      value: attrs.value || "",
      checked: "checked" in attrs,
      hidden: "hidden" in attrs,
      textContent: "",
      addEventListener(name, handler) { listeners.set(name, handler); },
      fire(name) { const handler = listeners.get(name); if (handler) handler(); },
    });
  }
  return nodes;
}

function slot() {
  let html = "";
  let nodes = [];
  return {
    get innerHTML() { return html; },
    set innerHTML(value) { html = value; nodes = parseNodes(value); },
    querySelector(selector) {
      const wanted = selector.replace(/^\./, "");
      return nodes.find((n) => n.className.split(" ").includes(wanted)) || null;
    },
  };
}

const OPTS = { idx: 0, help: () => "", escHtml: (s) => String(s) };
const WRITER = { write_capabilities: ["solar_pv"] };

function nibe(overrides) {
  return Object.assign({
    name: "nibe",
    lua: "drivers/nibe_local.lua",
    capabilities: { http: { allowed_hosts: ["192.168.2.178:8443"] } },
    config: { host: "192.168.2.178", username: "local-api" },
  }, overrides || {});
}

test("a driver that declares no write path gets no write markup at all", () => {
  const feed = load();
  const target = slot();
  assert.equal(feed.fill(target, nibe(), { capabilities: ["apicreds"] }, OPTS), false);
  assert.equal(target.innerHTML, "",
    "an inert hidden fieldset would still be read on save, writing a write-block " +
    "into the config of a driver that has no write path");
  assert.equal(feed.fill(target, nibe(), undefined, OPTS), false,
    "a catalog that has not resolved yet is not a licence to render the switch");
});

test("one switch moves both gates, because either alone does nothing", () => {
  const feed = load();
  const target = slot();
  feed.fill(target, nibe(), WRITER, OPTS);
  assert.match(target.innerHTML, /data-checkbox-path="drivers\.0\.config\.write\.solar_pv"/);
  assert.match(target.innerHTML, /data-checkbox-path="drivers\.0\.capabilities\.http\.allow_write"/);
  assert.match(target.innerHTML, /data-path="drivers\.0\.config\.write\.max_w"/);

  const toggle = target.querySelector(".drv-write-toggle");
  const grant = target.querySelector(".drv-write-grant");
  const ceiling = target.querySelector(".drv-write-max");
  assert.equal(toggle.checked, false, "a driver arrives read-only");
  assert.equal(grant.checked, false);

  ceiling.value = "9000";
  toggle.checked = true;
  toggle.fire("change");
  assert.equal(grant.checked, true, "the host grant follows the switch the operator sees");
});

test("no ceiling, no feed", () => {
  const feed = load();
  const target = slot();
  feed.fill(target, nibe(), WRITER, OPTS);
  const toggle = target.querySelector(".drv-write-toggle");
  const grant = target.querySelector(".drv-write-grant");
  const warning = target.querySelector(".drv-write-warning");

  toggle.checked = true;
  toggle.fire("change");
  assert.equal(toggle.checked, false, "the ceiling bounds every value FTW can send");
  assert.equal(grant.checked, false);
  assert.equal(warning.hidden, false);
  assert.match(warning.textContent, /maximum above 0 W/);
});

test("clearing the ceiling disarms a running feed", () => {
  const feed = load();
  const target = slot();
  feed.fill(target, nibe({
    capabilities: { http: { allow_write: true } },
    config: { host: "192.168.2.178", write: { solar_pv: true, max_w: 9000 } },
  }), WRITER, OPTS);
  const toggle = target.querySelector(".drv-write-toggle");
  const ceiling = target.querySelector(".drv-write-max");
  assert.equal(toggle.checked, true, "both gates set means the feed is armed");
  assert.equal(ceiling.value, "9000");

  ceiling.value = "";
  ceiling.fire("input");
  assert.equal(toggle.checked, false);
  assert.equal(target.querySelector(".drv-write-grant").checked, false);
});

test("a half-armed config reads as off, because it never wrote anything", () => {
  const feed = load();
  const target = slot();
  // config.write.solar_pv without the host grant: the driver disables the
  // feed on its own when host.http_patch is missing, so showing this as on
  // would claim something that was never true.
  feed.fill(target, nibe({
    config: { host: "192.168.2.178", write: { solar_pv: true, max_w: 9000 } },
  }), WRITER, OPTS);
  assert.equal(target.querySelector(".drv-write-toggle").checked, false);
  assert.equal(target.querySelector(".drv-write-grant").checked, false);
});

test("an armed config with no usable ceiling shows as off, with the reason", () => {
  const feed = load();
  const target = slot();
  feed.fill(target, nibe({
    capabilities: { http: { allow_write: true } },
    config: { host: "192.168.2.178", write: { solar_pv: true, max_w: 0 } },
  }), WRITER, OPTS);
  assert.equal(target.querySelector(".drv-write-toggle").checked, false,
    "the driver would refuse this config; the screen must not claim otherwise");
  assert.equal(target.querySelector(".drv-write-warning").hidden, false);
});

test("the panel states what has to be done on the pump", () => {
  const feed = load();
  const target = slot();
  feed.fill(target, nibe(), WRITER, OPTS);
  // Nothing in FTW can read the pump's installer menu, so an operator who
  // only ticks the box here would get silent per-point "read only value"
  // errors with no idea why.
  assert.match(target.innerHTML, /7\.5\.15/);
  assert.match(target.innerHTML, /Solar PV/);
  assert.match(target.innerHTML, /read only value/);
});

test("local-API setup does not send owners to an app they never used", () => {
  // The NIBE Local REST API account is generated on the pump's own screen.
  // The help text said myUplink, which is a different product with a
  // different account, and following it is a dead end.
  assert.ok(!/local-API account you set up in the myUplink app/.test(source));
  assert.match(source, /the account the pump generates on its own screen/);
});
