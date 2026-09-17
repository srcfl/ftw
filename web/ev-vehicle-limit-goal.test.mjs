import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const controlsSource = source.slice(source.indexOf('function evIsPaused'), source.indexOf('// sliderHeader builds')) +
  source.slice(source.indexOf('function sliderHeader'), source.indexOf('// buildEvPlanView')) +
  source.slice(source.indexOf('function buildScheduleSection'), source.indexOf('var evRefreshTimer'));

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.style = {}; this.dataset = {}; this.attrs = {}; this.events = {}; this.textContent = ''; }
  appendChild(el) { this.children.push(el); return el; }
  setAttribute(k, v) { this.attrs[k] = v; }
  addEventListener(k, fn) { (this.events[k] ??= []).push(fn); }
  emit(k) { for (const fn of this.events[k] ?? []) fn(); }
  cloneNode() { const copy = new Element(this.tag); copy.style = { ...this.style }; return copy; }
}
function descendants(el) { return [el, ...el.children.flatMap(descendants)]; }
const settle = () => new Promise(resolve => setImmediate(resolve));
function fixture({ supported = true, schedule = { soc: .8, time_of_day_min_utc: 420, recurring: true }, post } = {}) {
  const timers = new Map(), requests = [];
  let timerID = 0;
  const lp = { id: 'car/1', plugged_in: true, schedule, target_soc: 1 };
  const api = new Function('document', 'setTimeout', 'clearTimeout', 'evWrite', 'refreshEvModalAfterWrite',
    controlsSource + ';return { buildEvControls, buildScheduleSection };')(
    { createElement: tag => new Element(tag) },
    fn => { timers.set(++timerID, fn); return timerID; }, id => timers.delete(id),
    (path, options) => { const request = { path, ...JSON.parse(options.body) }; requests.push(request); return post ? post(request) : Promise.resolve({ ok: true }); },
    () => {},
  );
  const root = api.buildEvControls(lp, false, supported);
  const find = predicate => descendants(root).find(predicate);
  return {
    root, requests, lp, find,
    mode: find(el => el.attrs['aria-label'] === 'Charge target'),
    slider: find(el => el.attrs['aria-label'] === 'Target charge, percent'),
    time: find(el => el.attrs['aria-label'] === 'Ready by'),
    text: () => descendants(root).map(el => el.textContent).join('\n'),
    async flush() { for (const [id, fn] of timers) { timers.delete(id); fn(); } await settle(); },
  };
}

test('existing percent goal stays unchanged on open and a time edit', async () => {
  const ui = fixture({ schedule: { soc: .73, time_of_day_min_utc: 420, recurring: true } });
  assert.equal(ui.mode.value, 'percent');
  assert.equal(ui.slider.hidden, false);
  await ui.flush();
  assert.equal(ui.requests.length, 0);
  ui.time.value = '08:15'; ui.time.emit('change'); await ui.flush();
  assert.equal(ui.requests[0].schedule.soc, .73);
  assert.equal(ui.requests[0].schedule.finish_at_vehicle_limit, false);
});

test('car-limit selection preserves percent and selected weekdays', async () => {
  const ui = fixture({ schedule: { soc: .85, time_of_day_min_utc: 420, recurring: true, days: 31 } });
  ui.mode.value = 'vehicle'; ui.mode.emit('change');
  assert.equal(ui.slider.hidden, true);
  assert.equal(ui.slider.disabled, true);
  const details = ui.find(el => el.tag === 'details' && el.children[0]?.textContent === 'How this goal works');
  assert.equal(details.hidden, false);
  assert.notEqual(details.open, true);
  await ui.flush();
  assert.equal(ui.requests[0].path, '/api/loadpoints/car%2F1/target');
  assert.equal(ui.requests[0].schedule.finish_at_vehicle_limit, true);
  assert.equal(ui.requests[0].schedule.soc, .85);
  assert.equal(ui.requests[0].schedule.days, 31);
  ui.mode.value = 'percent'; ui.mode.emit('change'); await ui.flush();
  assert.equal(ui.requests[1].schedule.finish_at_vehicle_limit, false);
  assert.equal(ui.requests[1].schedule.soc, .85);
  ui.slider.value = '90'; ui.slider.emit('change'); await ui.flush();
  assert.equal(ui.requests[2].schedule.soc, .9);
});

test('only an explicit true support flag exposes the choice', async () => {
  for (const supported of [false, undefined, 'true', 1]) {
    const ui = fixture({ supported: supported === undefined ? null : supported });
    assert.equal(ui.mode, undefined);
    ui.time.value = '08:15'; ui.time.emit('change'); await ui.flush();
    assert.equal('finish_at_vehicle_limit' in ui.requests[0].schedule, false);
    assert.equal(ui.requests[0].schedule.soc, .8);
  }
});

test('a car-limit goal with no percent is a saved goal, not runtime 100 percent', async () => {
  const ui = fixture({ schedule: { soc: 0, finish_at_vehicle_limit: true, time_of_day_min_utc: 420 } });
  assert.equal(ui.mode.value, 'vehicle');
  assert.equal(ui.slider.hidden, true);
  assert.match(ui.text(), /Car's charge limit by/);
  assert.doesNotMatch(ui.text(), /100 % by|No ready time set|No goal set yet/);
  assert.equal(ui.find(el => el.textContent === 'Remove schedule').hidden, false);
  ui.time.value = '08:15'; ui.time.emit('change'); await ui.flush();
  assert.equal(ui.requests[0].schedule.finish_at_vehicle_limit, true);
  assert.equal(ui.requests[0].schedule.soc, 0);
});

test('a failed save stays unconfirmed and does not change the saved summary', async () => {
  const ui = fixture({ post: async () => ({ ok: false, status: 500, json: async () => ({ error: 'Could not save goal' }) }) });
  ui.mode.value = 'vehicle'; ui.mode.emit('change');
  assert.match(ui.text(), /Applying schedule/);
  await ui.flush();
  assert.match(ui.text(), /Schedule not confirmed: Could not save goal/);
  assert.match(ui.text(), /80 % by/);
  assert.doesNotMatch(ui.text(), /Schedule saved/);
});

test('quick mode edits only save the last choice', async () => {
  const ui = fixture();
  ui.mode.value = 'vehicle'; ui.mode.emit('change');
  ui.mode.value = 'percent'; ui.mode.emit('change');
  await ui.flush();
  assert.equal(ui.requests.length, 1);
  assert.equal(ui.requests[0].schedule.finish_at_vehicle_limit, false);
});

test('Charge now sends a manual hold without a percent release in car-limit mode', async () => {
  const ui = fixture({ schedule: { soc: .8, finish_at_vehicle_limit: true } });
  ui.find(el => el.textContent === 'Charge now').emit('click'); await settle();
  assert.equal(ui.requests[0].path, '/api/loadpoints/car%2F1/manual_hold');
  assert.equal(ui.requests[0].hold_s, 0);
  assert.equal(ui.requests[0].power_w, 11040);
  assert.equal('release_at_soc_pct' in ui.requests[0], false);
});
