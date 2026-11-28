import assert from "node:assert/strict";
import {test} from "node:test";
import {formatEnergyToday, updateEnergyToday} from "./energy-today.js";

test("today distinguishes a complete zero, a measured subtotal and no evidence", () => {
  assert.equal(formatEnergyToday({pv_wh: 0}, "pv_wh"), "0.00 kWh");
  assert.equal(formatEnergyToday({pv_wh: null, measured: {pv_wh: 0}}, "pv_wh"), "≥ 0.00 kWh");
  assert.equal(formatEnergyToday({pv_wh: null, measured: {pv_wh: 600}}, "pv_wh"), "≥ 0.60 kWh");
  for (const invalid of [undefined, null, NaN, Infinity, -10, "600"]) {
    assert.equal(formatEnergyToday({pv_wh: null, measured: {pv_wh: invalid}}, "pv_wh"), "Unknown");
  }
  assert.equal(formatEnergyToday({pv_wh: 1200, measured: {pv_wh: 600}}, "pv_wh"), "1.20 kWh");
});

test("the energy cards and overview mark subtotals and clear the mark after a complete reading", () => {
  const elements = new Map();
  const group = {append(el) { elements.set(el.id, el); }};
  for (const id of ["e-pv", "e-load", "overview-e-import", "overview-e-pv"]) {
    const classes = new Set();
    elements.set(id, {
      closest() { return group; },
      classList: {toggle(name, on) { if (on) classes.add(name); else classes.delete(name); }, contains: (name) => classes.has(name)},
    });
  }
  const root = {getElementById(id) { return elements.get(id); }, createElement() { return {}; }};
  updateEnergyToday({pv_wh: null, load_wh: null, measured: {pv_wh: 600, load_wh: 1200}}, root);
  assert.equal(elements.get("e-pv").textContent, "≥ 0.60 kWh");
  assert.equal(elements.get("overview-e-pv").textContent, "≥ 0.60 kWh");
  assert.equal(elements.get("e-load").textContent, "≥ 1.20 kWh");
  assert.match(elements.get("e-pv").title, /missing readings/);
  assert.equal(elements.get("energy-today-quality").hidden, false);

  const complete = {pv_wh: 1000, load_wh: 2000, import_wh: 500, export_wh: 0, bat_charged_wh: 0, bat_discharged_wh: 0};
  updateEnergyToday(complete, root);
  assert.equal(elements.get("e-pv").textContent, "1.00 kWh");
  assert.equal(elements.get("e-pv").title, "");
  assert.equal(elements.get("energy-today-quality").hidden, true);
  updateEnergyToday({...complete, history_quality: "estimated"}, root);
  assert.equal(elements.get("e-load").textContent, "≈ 2.00 kWh");
  assert.equal(elements.get("e-pv").textContent, "1.00 kWh");
  assert.match(elements.get("e-load").title, /Estimated/);
  assert.equal(elements.get("energy-today-quality").hidden, false);
  assert.match(elements.get("energy-today-quality").textContent, /Home use includes estimates/);
  updateEnergyToday(null, root);
  assert.equal(elements.get("e-pv").textContent, "Unknown");
  // No reading is not an import: the red import colour gives way.
  assert.equal(elements.get("overview-e-import").textContent, "Unknown");
  assert.equal(elements.get("overview-e-import").classList.contains("is-unknown"), true);
  assert.equal(elements.get("energy-today-quality").textContent, "Today's energy is not available yet.");
  updateEnergyToday(complete, root);
  assert.equal(elements.get("overview-e-import").classList.contains("is-unknown"), false);
});
