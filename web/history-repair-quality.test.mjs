import assert from "node:assert/strict";
import {test} from "node:test";
import {formatEnergyToday, updateEnergyToday} from "./energy-today.js";
import {flowReadingsFromStatus} from "./components/energy-flow-readings.js";

test("corrected household energy stays estimated even when another original flow is missing",()=>{
  const today={load_wh:28000,load_quality:"estimated",history_quality:"incomplete",pv_wh:null,import_wh:12000,measured:{load_wh:null}};
  assert.equal(formatEnergyToday(today,"load_wh"),"≈ 28.0 kWh");
  assert.equal(formatEnergyToday(today,"pv_wh"),"Unknown");
  assert.equal(flowReadingsFromStatus({load_w:1200,energy:{today}}).selfPoweredPctToday,null);
  assert.equal(today.measured.load_wh,null);
});

test("endpoint solar and home totals both show estimate markers",()=>{
  const today={load_wh:41000,load_quality:"estimated",pv_wh:37000,pv_quality:"estimated",history_quality:"incomplete",measured:{load_wh:null,pv_wh:null}};
  assert.equal(formatEnergyToday(today,"load_wh"),"≈ 41.0 kWh");
  assert.equal(formatEnergyToday(today,"pv_wh"),"≈ 37.0 kWh");
});


test("a complete solar estimate explains its quality without claiming missing totals", () => {
  const elements = new Map();
  const group = {append(el) { elements.set(el.id, el); }};
  for (const id of ["e-pv", "e-load"]) elements.set(id, {closest() { return group; }});
  const root = {getElementById(id) { return elements.get(id); }, createElement() { return {}; }};
  const complete = {pv_wh: 1000, load_wh: 2000, import_wh: 500, export_wh: 0,
    bat_charged_wh: 0, bat_discharged_wh: 0, pv_quality: "estimated"};
  updateEnergyToday(complete, root);
  const note = elements.get("energy-today-quality");
  assert.equal(note.hidden, false);
  assert.equal(note.textContent, "Solar includes estimated energy across historical gaps.");
  assert.equal(elements.get("e-pv").textContent, "≈ 1.00 kWh");
  updateEnergyToday({...complete, load_quality: "estimated"}, root);
  assert.equal(note.textContent, "Home use includes estimates from source energy. Solar includes estimated energy across historical gaps.");
  updateEnergyToday({...complete, import_wh: null}, root);
  assert.match(note.textContent, /^Gaps in today's readings/);
  assert.match(note.textContent, /Solar includes estimated energy/);
});
