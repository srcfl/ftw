// An owner picking a driver sees how many homes run that exact version.
// That count is all FTW says about a driver. Settings and the setup guide
// show the same card, from device-flow.js.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const flowSource = readFileSync(new URL("./device-flow.js", import.meta.url), "utf8");
const setup = readFileSync(new URL("./setup.js", import.meta.url), "utf8");

function element(tag) {
  return {
    tag, children: [], className: "", markup: [],
    _text: "",
    set textContent(v) { this._text = String(v); },
    get textContent() { return this._text; },
    set innerHTML(v) { this.markup.push(v); },
    appendChild(child) { this.children.push(child); return child; },
    addEventListener() {},
  };
}

function deviceFlow() {
  const window = {};
  vm.runInNewContext(flowSource, { window, document: { createElement: element } });
  return window.FTWDeviceFlow;
}

const texts = (el) => [el.textContent, ...el.children.flatMap(texts)].filter(Boolean);

test("a count is turned into words", () => {
  const flow = deviceFlow();
  assert.equal(flow.usageLabel(1), "1 home runs this version");
  assert.equal(flow.usageLabel(14), "14 homes run this version");
  assert.equal(flow.usageLabel(undefined), "", "no count, no words");
  assert.ok(!flowSource.includes("verified on hardware"), "no status label is left");
  assert.ok(!flowSource.includes("untested"), "no status label is left");
});

test("the card carries the count, the version and the models", () => {
  const card = deviceFlow().card({
    id: "sungrow", name: "Sungrow SH Hybrid Inverter", manufacturer: "Sungrow", version: "1.4.0",
    protocols: ["modbus"], capabilities: ["meter"], run_by: 9, tested_models: ["SH10RT"],
  });
  const all = texts(card);
  assert.ok(all.includes("9 homes run this version"));
  assert.ok(all.includes("modbus · v1.4.0 · SH10RT"));
  assert.equal(card.tag, "button");
});

test("the card is built as elements, not markup from the driver file", () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const card = deviceFlow().card({ id: "x", name: hostile, setup_hint: hostile, tested_models: [hostile] });
  const walk = (el) => [el, ...el.children.flatMap(walk)];
  for (const el of walk(card)) assert.equal(el.markup.length, 0, "no innerHTML on a card");
  assert.ok(texts(card).includes(hostile), "the name stays text");
});

test("the guide shows the shared card", () => {
  assert.match(setup, /flow\(\)\.card\(entry, \{/);
  assert.doesNotMatch(setup, /function usageLabel\(count\)/, "the guide keeps no copy of the wording");
});
