import assert from "node:assert/strict";
import {test} from "node:test";
import {energyTodayReading, formatEnergyToday, updateEnergyToday} from "./energy-today.js";

const subtotal = {wh: 26603, coverage_ms: 48900000, period_ms: 50400000,
  minimum_bucket_ms: 300000, quality: "estimated", method: "accepted_interval_balance_v1"};
const incomplete = {load_wh: null, history_quality: "incomplete",
  measured: {load_wh: null}, load_subtotal: subtotal};

test("a source-integrated partial day displays its own estimated subtotal, never a complete total", () => {
  assert.equal(formatEnergyToday(incomplete, "load_wh"), "≈ 26.6 kWh (partial)");
  assert.equal(incomplete.load_wh, null);
  assert.equal(incomplete.measured.load_wh, null);
  assert.equal(formatEnergyToday({...incomplete, load_wh: 30000, history_quality: "estimated"}, "load_wh"), "≈ 30.0 kWh");
  assert.equal(formatEnergyToday({...incomplete, load_wh: 0}, "load_wh"), "0.00 kWh");
  assert.equal(formatEnergyToday({...incomplete, load_subtotal: {...subtotal, wh: 0}}, "load_wh"), "≈ 0.00 kWh (partial)");
  assert.equal(formatEnergyToday(incomplete, "pv_wh"), "Unknown");
});

test("missing or malformed interval evidence cannot manufacture a partial zero", () => {
  for (const invalid of [undefined, null, {...subtotal, wh: NaN}, {...subtotal, wh: -1},
    {...subtotal, coverage_ms: 0}, {...subtotal, coverage_ms: subtotal.period_ms + 1},
    {...subtotal, period_ms: "50400000"}, {...subtotal, quality: "measured"},
    {...subtotal, method: "interpolated"}]) {
    assert.equal(energyTodayReading({...incomplete, load_subtotal: invalid}, "load_wh").wh, null);
  }
});

test("cards explain accepted coverage and clear the partial state after recovery or missing data", () => {
  const elements = new Map();
  const group = {append(el) { elements.set(el.id, el); }};
  for (const id of ["e-load", "e-pv"]) elements.set(id, {closest() { return group; }});
  const root = {getElementById(id) { return elements.get(id); }, createElement() { return {}; }};
  updateEnergyToday(incomplete, root);
  assert.equal(elements.get("e-load").textContent, "≈ 26.6 kWh (partial)");
  assert.match(elements.get("e-load").title, /gaps are excluded/);
  assert.match(elements.get("energy-today-quality").textContent, /815 of 840 min/);
  const complete = {load_wh: 30000, pv_wh: 10000, import_wh: 10000, export_wh: 0,
    bat_charged_wh: 0, bat_discharged_wh: 0};
  updateEnergyToday(complete, root);
  assert.equal(elements.get("e-load").textContent, "30.0 kWh");
  assert.equal(elements.get("e-load").title, "");
  assert.equal(elements.get("energy-today-quality").hidden, true);
  updateEnergyToday(null, root);
  assert.equal(elements.get("e-load").textContent, "Unknown");
  assert.equal(elements.get("energy-today-quality").textContent, "Today's energy is not available yet.");
});
