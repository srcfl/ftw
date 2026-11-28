import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

import { deviceFreshness, paintOverviewFreshness } from "./overview-freshness.js";

// /api/status rows as Core sends them with one driver on a wrong port: the
// emit status still says ok for the first polls, src_state says never.
const meter = { status: "ok", src_state: "live", consecutive_errors: 0, last_ok_age_ms: 900 };
const wrongPort = {
  status: "ok", src_state: "never", consecutive_errors: 2,
  last_error: "driver_poll: the device refused all 17 modbus reads",
};

describe("overview freshness", () => {
  it("is live only while every configured device answers", () => {
    assert.deepEqual(deviceFreshness({ drivers: { meter } }), { live: true, quiet: [] });
    const f = deviceFreshness({ drivers: { meter, sungrow: wrongPort } });
    assert.equal(f.live, false);
    assert.deepEqual(f.quiet, [{
      name: "sungrow", says: "has never answered", detail: "the device refused all 17 modbus reads",
    }]);
  });

  it("names each kind of silence with what Core knows", () => {
    const f = deviceFreshness({ drivers: {
      a: { status: "offline", src_state: "down", consecutive_errors: 40, last_ok_age_ms: 4 * 60_000, last_error: "x" },
      b: { status: "offline", not_running: true, src_state: "never" },
      c: { status: "fault", src_state: "down", device_fault: true, device_fault_reason: "EnergyHub Fault Mode" },
      d: { status: "offline", src_state: "stale", last_ok_age_ms: 3 * 3_600_000 },
      // Unplugged: its reads fail and the commands it missed made Core mark
      // it faulted. Not answering is the fact that matters.
      e: { status: "fault", src_state: "down", device_fault: true, consecutive_errors: 19,
        device_fault_reason: "driver command may have run:\ndriver_command returned false",
        last_ok_age_ms: 94_000, last_error: "driver_poll: the device refused all 17 modbus reads" },
      off: { status: "disabled", disabled: true },
    } });
    assert.equal(f.live, false);
    assert.deepEqual(f.quiet.map((r) => r.name + " " + r.says), [
      "a has not answered for 4 min",
      "b did not start",
      "c answers but cannot act",
      "d has not answered for 3 h",
      "e has not answered for 2 min",
    ]);
    assert.equal(f.quiet[2].detail, "EnergyHub Fault Mode");
    assert.equal(f.quiet[4].detail, "the device refused all 17 modbus reads");
  });

  it("keeps LIVE dark but names nobody before a device's first poll ends", () => {
    const f = deviceFreshness({ drivers: { meter, starting: { status: "ok", src_state: "never", consecutive_errors: 0 } } });
    assert.deepEqual(f, { live: false, quiet: [] });
    // A late reading is not yet a quiet device.
    assert.deepEqual(deviceFreshness({ drivers: { slow: { status: "ok", src_state: "lagging" } } }), { live: false, quiet: [] });
  });

  it("ignores a disabled device and calls a box with no devices live, as the apps do", () => {
    assert.deepEqual(deviceFreshness({ drivers: { off: { status: "disabled", disabled: true } } }), { live: true, quiet: [] });
    assert.deepEqual(deviceFreshness({}), { live: true, quiet: [] });
  });

  it("paints a ring, not a green dot, and the card with the device's name", () => {
    const doc = fakeDocument();
    let opened = 0;
    paintOverviewFreshness(doc, { drivers: { meter, sungrow: wrongPort } }, () => opened++);
    const pill = doc.getElementById("overview-health");
    assert.equal(pill.classList.contains("is-connected"), true);
    assert.equal(pill.classList.contains("is-live"), false);
    assert.equal(doc.getElementById("overview-health-label").textContent, "Not live");
    const card = doc.getElementById("overview-quiet");
    assert.equal(card.hidden, false);
    assert.match(card.text(), /sungrow has never answered/);
    assert.match(card.text(), /the device refused all 17 modbus reads/);
    card.find("button").click();
    assert.equal(opened, 1);

    // The same answer again leaves the card (and the button's focus) alone.
    const before = card.children;
    paintOverviewFreshness(doc, { drivers: { meter, sungrow: wrongPort } }, () => opened++);
    assert.equal(card.children, before);

    paintOverviewFreshness(doc, { drivers: { meter, sungrow: { ...meter } } }, () => opened++);
    assert.equal(pill.classList.contains("is-live"), true);
    assert.equal(doc.getElementById("overview-health-label").textContent, "Live");
    assert.equal(card.hidden, true);
  });

  it("is wired into the page: the card exists and the green dot needs is-live", () => {
    const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
    const css = readFileSync(new URL("./app.css", import.meta.url), "utf8");
    const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
    assert.match(html, /id="overview-quiet"[^>]*hidden/);
    assert.match(css, /\.overview-health\.is-live \.overview-health-dot \{\s*background: var\(--live\)/);
    assert.doesNotMatch(css, /\.overview-health\.is-connected \.overview-health-dot \{\s*background: var\(--live\)/);
    assert.match(app, /setConnected\(true, data\)/);
    assert.doesNotMatch(app, /overviewHealthLabel\.textContent = "Live"/);
  });
});

function fakeElement(tag) {
  const classes = new Set();
  const listeners = {};
  const el = {
    tag, hidden: false, textContent: "", dataset: {}, children: [], type: "",
    classList: {
      add: (...n) => n.forEach((c) => classes.add(c)),
      toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)),
      contains: (c) => classes.has(c),
    },
    append(...kids) { el.children = [...el.children, ...kids]; },
    replaceChildren(...kids) { el.children = kids; },
    addEventListener(type, fn) { listeners[type] = fn; },
    click() { listeners.click && listeners.click(); },
    text() {
      return el.textContent + el.children.map((k) => (typeof k === "string" ? k : k.text())).join("");
    },
    find(t) {
      for (const k of el.children) {
        if (typeof k === "string") continue;
        if (k.tag === t) return k;
        const hit = k.find(t);
        if (hit) return hit;
      }
      return null;
    },
  };
  return el;
}

function fakeDocument() {
  const byId = {
    "overview-health": fakeElement("div"),
    "overview-health-label": fakeElement("span"),
    "overview-quiet": fakeElement("section"),
  };
  byId["overview-quiet"].hidden = true;
  return { getElementById: (id) => byId[id] || null, createElement: (t) => fakeElement(t) };
}
