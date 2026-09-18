import test from 'node:test';
import assert from 'node:assert/strict';
import { chargingLevels, chargingPlan, plannedEVWatts, createChargingTimeline } from './ev-plan.js';
import { derivePlanBrief } from './plan-brief.js';
import { readFileSync } from 'node:fs';

const hour = 3600000, now = Date.UTC(2026,8,17,20);
const car = { plugged_in: true, current_soc: .45, soc_source: 'inferred', schedule: { finish_at_vehicle_limit: true, soc: .8 } };
const known = { ...car, vehicle_driver: 'car', vehicle_soc: .47, vehicle_charge_limit: .8, soc_source: 'vehicle' };

test('reported battery level is distinct from the controller estimate and target', () => {
  const info = chargingLevels(known);
  assert.equal(info.now, '47%'); assert.equal(info.limit, '80%');
  assert.equal(info.source, 'Reported by car'); assert.equal(info.fromCar, true);
  assert.match(info.explanation, /47% to 80%/);
  assert.equal(chargingLevels({ ...known, vehicle_soc: 0 }).now, '0%');
  assert.equal(chargingLevels({ ...known, vehicle_soc: undefined }).now, '0%', 'Core omits zero vehicle_soc');
});

test('stale, missing or unbound car telemetry cannot become a known car limit', () => {
  for (const change of [{ vehicle_driver: '' }, { vehicle_stale: true }, { vehicle_charge_limit: 0 }, { vehicle_charge_limit: 1.2 }]) {
    const info = chargingLevels({ ...known, ...change });
    assert.equal(info.limit, 'Unknown'); assert.match(info.explanation, /up to 100%/);
  }
  const info = chargingLevels({ ...car, target_soc: 1 });
  assert.equal(info.limit, 'Unknown'); assert.equal(info.now, '45%');
  assert.equal(info.source, 'Estimated by FTW');
  assert.equal(chargingLevels({ ...car, soc_source: 'assumed' }).source, 'Needs confirmation');
  assert.equal(chargingLevels({ ...car, soc_source: 'completed' }).source, 'Needs confirmation');
  assert.equal(chargingLevels({ ...known, read_unavailable: true }).now, 'Unknown');
});

test('percent goals stay separate from the car limit and respect a lower car cap', () => {
  const lp = { ...known, schedule: { soc: .7 } };
  assert.equal(chargingLevels(lp).limit, '80%');
  assert.match(chargingLevels(lp).explanation, /to 70%/);
  assert.match(chargingLevels({ ...lp, schedule: { soc: .9 } }).explanation, /to 80%/);
});

test('per-car powers win over legacy aggregate without double counting', () => {
  assert.equal(plannedEVWatts({ loadpoint_power_w: { a: 3000, b: 5000 }, loadpoint_w: 8000 }), 8000);
  assert.equal(plannedEVWatts({ loadpoint_power_w: { a: 0 }, loadpoint_w: 8000 }), 0);
  assert.equal(plannedEVWatts({ loadpoint_power_w: {}, loadpoint_w: 3200 }), 3200);
  assert.equal(plannedEVWatts({ loadpoint_power_w: { a: NaN, b: -20, c: 100 } }), 100);
});

test('household brief finds a future multi-car slot and names its total power', () => {
  const brief = derivePlanBrief({ enabled: true, now, status: { mode: 'planner_active' }, plan: { actions: [
    { slot_start_ms: now, slot_len_min: 15, battery_w: 0 },
    { slot_start_ms: now + hour, slot_len_min: 15, loadpoint_power_w: { a: 3000, b: 4000 } },
  ] } });
  assert.equal(brief.next.action, 'Charge EV at 7.0 kW');
  assert.match(brief.next.time, /^At /);
});

const windows = [
  { start_ms: now + 8 * hour, end_ms: now + 10 * hour, wh: 14000 },
  { start_ms: now - hour, end_ms: now + hour, wh: 14000 },
  { start_ms: now - 3 * hour, end_ms: now - 2 * hour, wh: 7000 },
  { start_ms: now + 23 * hour, end_ms: now + 25 * hour, wh: 14000 },
];
test('24-hour windows are sorted and clipped; partial energy is marked approximate', () => {
  const plan = chargingPlan({ ...car, plan_windows: windows }, now);
  assert.equal(plan.windows.length, 3);
  assert.equal(plan.windows[0].start_ms, now);
  assert.equal(plan.windows.at(-1).end_ms, now + 24 * hour);
  assert.equal(plan.wh, 28000); assert.equal(plan.approximate, true);
  const invalid = chargingPlan({ ...car, plan_windows: [{ start_ms: now, end_ms: now, wh: 5 }] }, now);
  assert.equal(invalid.windows.length, 0);
});

test('unavailable, manual, complete and updating states hide old windows', () => {
  for (const change of [
    { manual_active: true }, { manual_active: true, manual_charge_w: 0 },
    { goal_complete: true }, { plan_pending: true }, { plan_outdated: true },
    { surplus_only: true }, { grid_deferred: true }, { plugged_in: false },
    { charger: { available: false } }, { power_unavailable: true },
  ]) {
    const plan = chargingPlan({ ...car, plan_windows: windows, ...change }, now);
    assert.equal(plan.windows.length, 0, JSON.stringify(change)); assert.ok(plan.message);
  }
  assert.match(chargingPlan(null, now).message, /unavailable/);
  assert.match(chargingPlan(car, now).message, /No charging planned/);
});

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.style = {}; this.events = {}; this.attrs = {}; this.textContent = ''; }
  appendChild(el) { el.parentNode = this; this.children.push(el); return el; }
  insertBefore(el, ref) { el.parentNode = this; this.children.splice(this.children.indexOf(ref),0,el); }
  replaceChild(el, old) { el.parentNode=this; this.children.splice(this.children.indexOf(old),1,el); }
  removeChild(el) { this.children.splice(this.children.indexOf(el),1); }
  replaceChildren() { this.children=[]; }
  setAttribute(k,v) { this.attrs[k]=v; }
  addEventListener(k,fn) { (this.events[k]??=[]).push(fn); }
  emit(k) { for(const fn of this.events[k]??[]) fn(); }
}
const doc = { createElement: tag => new Element(tag) };
const descendants = el => [el,...el.children.flatMap(descendants)];
const text = el => descendants(el).map(e=>e.textContent).join(' ');

test('timeline renders accessible time blocks, then clears them on an outdated plan', () => {
  const view = createChargingTimeline(doc);
  view.update({ ...car, plan_windows: windows }, { start: now });
  assert.match(text(view.el), /28.0 kWh/);
  assert.equal(descendants(view.el).filter(el=>el.className==='ev-plan-slot').length,3);
  assert.equal(descendants(view.el).find(el=>el.className==='ev-plan-more').hidden,false);
  view.update({ ...car, plan_windows: windows, plan_outdated: true }, { start: now });
  assert.match(text(view.el), /times unavailable/);
  assert.equal(descendants(view.el).filter(el=>el.className==='ev-plan-slot').length,0);
});

test('mounted car view follows source changes and keeps the slider while editing', async () => {
  const source=readFileSync(new URL('./app.js',import.meta.url),'utf8');
  const functions=source.slice(source.indexOf('function sliderHeader'),source.indexOf('function buildEvCapacityView'));
  const api=new Function('document','evPlanUI','buildEvCapacityView','renderEvPlanStatus','evWrite','refreshEvModalAfterWrite',functions+';return buildEvPlanView;')(
    doc, Promise.resolve({ chargingLevels, createChargingTimeline:()=>createChargingTimeline(doc) }),
    ()=>({el:new Element('details'),update(){}}),()=>null,()=>Promise.resolve({ok:true,json:async()=>({ok:true})}),async()=>{},
  );
  const view=api(known,{}); await new Promise(r=>setImmediate(r));
  assert.match(text(view.el),/47% Reported by car/);
  assert.equal(view.slider.parentNode.hidden,true);
  view.update(car,{});
  assert.equal(view.slider.parentNode.hidden,false);
  assert.equal(view.slider.value,'45');
  view.slider.value='55'; view.slider.emit('pointerdown');
  view.update({...car,current_soc:.46},{});
  assert.equal(view.slider.value,'55');
  view.slider.emit('pointerup'); view.update({...car,current_soc:.46},{});
  assert.equal(view.slider.value,'46');
});
