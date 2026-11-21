// node --test web/car-goal-editor.test.mjs
//
// One car's goal from the cars card (docs/cars.md): the EV modal's goal
// editor and solar rule, saved with the car through car.goal.set's route,
// so a car that is away gets its goal too.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import * as ui from './cars.js';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const between = (from, to) => source.slice(source.indexOf(from), source.indexOf(to));
const editorSource = between('function evIsPaused', '// sliderHeader builds') +
  between('function sliderHeader', '// buildEvPlanView') +
  between('function buildPVModeSection', '// buildScheduleSection') +
  between('function buildScheduleSection', 'var evRefreshTimer');

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.style = {}; this.dataset = {}; this.attrs = {}; this.events = {}; this.textContent = ''; }
  appendChild(el) { this.children.push(el); return el; }
  setAttribute(k, v) { this.attrs[k] = v; }
  addEventListener(k, fn) { (this.events[k] ??= []).push(fn); }
  emit(k) { for (const fn of this.events[k] ?? []) fn(); }
}
const descendants = el => [el, ...el.children.flatMap(descendants)];
const settle = () => new Promise(resolve => setImmediate(resolve));

const away = { id: 'b'.repeat(32), name: 'XC40', capacity_wh: 69000, surplus_only: false };

function fixture(car, { hasPV = true, held = car } = {}) {
  const timers = new Map(), requests = [], rebuilt = [];
  let timerID = 0, refreshes = 0;
  const api = new Function('document', 'setTimeout', 'clearTimeout', 'evWrite', 'refreshEvModalAfterWrite',
    editorSource + ';return { buildCarGoal };')(
    { createElement: tag => new Element(tag) },
    fn => { timers.set(++timerID, fn); return timerID; }, id => timers.delete(id),
    (path, options) => { requests.push({ path, method: options.method, body: JSON.parse(options.body) }); return Promise.resolve({ ok: true }); },
    () => { throw new Error('a car goal must not refresh the EV modal'); },
  );
  const root = api.buildCarGoal(ui, car, hasPV, {
    refresh: () => { refreshes++; return Promise.resolve(typeof held === 'function' ? held() : held); },
    rebuild: next => rebuilt.push(next),
  });
  const find = predicate => descendants(root).find(predicate);
  return {
    root, requests, rebuilt, find, refreshes: () => refreshes,
    time: find(el => el.attrs['aria-label'] === 'Ready by'),
    repeat: find(el => el.tag === 'label' && el.children[1]?.textContent === 'Repeat on chosen days')?.input,
    solar: find(el => el.attrs.role === 'switch'),
    text: () => descendants(root).map(el => el.textContent).join('\n'),
    async flush() { for (const [id, fn] of timers) { timers.delete(id); fn(); } await settle(); await settle(); },
  };
}

test('an away car gets a goal through its own route, saved with the car', async () => {
  const ui1 = fixture(away);
  assert.match(ui1.text(), /Saved with the car\. It applies wherever the car plugs in\./);
  assert.match(ui1.text(), /No goal/);
  // The site's battery rule for spare solar is the charger's, not the car's.
  assert.doesNotMatch(ui1.text(), /Solar timing|Home battery ≥/);
  ui1.time.value = '06:30'; ui1.time.emit('change'); await ui1.flush();
  assert.equal(ui1.requests.length, 1);
  assert.deepEqual(ui1.requests[0], {
    path: `/api/cars/${away.id}/goal`, method: 'PUT',
    body: { target: 0.8, ready_by_min: 390, days: 0, finish_at_limit: false },
  });
  assert.equal(ui1.refreshes(), 1, 'the editor reads the car back after the save');
});

test('a repeating goal keeps its weekdays', async () => {
  const car = { ...away, goal: { target_permille: 700, ready_by_min: 420, days: 0x1f, finish_at_limit: false } };
  const ui1 = fixture(car);
  assert.equal(ui1.time.value, '07:00');
  assert.equal(ui1.repeat.checked, true);
  ui1.time.value = '07:15'; ui1.time.emit('change'); await ui1.flush();
  assert.deepEqual(ui1.requests[0].body, { target: 0.7, ready_by_min: 435, days: 0x1f, finish_at_limit: false });
  ui1.repeat.checked = false; ui1.repeat.emit('change'); await ui1.flush();
  assert.equal(ui1.requests[1].body.days, 0, 'once');
});

test('Only spare solar is the car’s choice', async () => {
  const saved = { ...away, surplus_only: true };
  const ui1 = fixture(away, { held: saved });
  assert.equal(ui1.solar.checked, false);
  ui1.solar.checked = true; ui1.solar.emit('change'); await ui1.flush();
  assert.deepEqual(ui1.requests[0], { path: `/api/cars/${away.id}/goal`, method: 'PUT', body: { surplus_only: true } });
  assert.match(ui1.text(), /No grid or home battery/);
  assert.match(ui1.text(), /Only spare solar is on\. Your target may not be reached in time\./);
});

test('a site without solar shows the choice only for a car that already has it', () => {
  assert.equal(fixture(away, { hasPV: false }).solar, undefined);
  assert.ok(fixture({ ...away, surplus_only: true }, { hasPV: false }).solar);
});

test('removing the goal clears it on the car and redraws from the car read back', async () => {
  const car = { ...away, goal: { target_permille: 800, ready_by_min: 420, days: 127, finish_at_limit: false } };
  const cleared = { ...away };
  const ui1 = fixture(car, { held: cleared });
  ui1.find(el => el.textContent === 'Remove schedule').emit('click');
  await ui1.flush();
  assert.deepEqual(ui1.requests[0].body, { clear: true });
  assert.deepEqual(ui1.rebuilt, [cleared]);
});
