import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const section = source.slice(
  source.indexOf('function evIsPaused'),
  source.indexOf('function sliderHeader'),
);

// Core treats a Charge now hold as a ceiling and floors it to the charger's
// step table within max_charge_w. The slider must stop where Core does: a
// one-phase charger rated 32 A with a 16 A table showed 6–32 A and
// "32 A · 7.4 kW" while Core ordered 3,680 W (22 Oct 2026 rig).

class FakeEl {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.style = {};
    this.children = [];
    this.attributes = {};
    this.listeners = {};
    this.textContent = '';
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  appendChild(c) { this.children.push(c); return c; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  cloneNode() { const c = new FakeEl(this.tagName); Object.assign(c.style, this.style); return c; }
  fire(type) { for (const fn of this.listeners[type] || []) fn({ type }); }
}

function walk(el, out = []) {
  out.push(el);
  for (const c of el.children) walk(c, out);
  return out;
}

function build(lp) {
  const sent = [];
  const evWrite = (path, opts) => { sent.push(JSON.parse(opts.body)); return new Promise(() => {}); };
  const make = new Function('document', 'evWrite', 'refreshEvModalAfterWrite', section + '; return buildManualChargeSection;');
  const ctl = make({ createElement: tag => new FakeEl(tag) }, evWrite, () => {})(lp);
  const all = walk(ctl.el);
  const slider = all.find(el => el.tagName === 'INPUT' && el.type === 'range');
  const row = all.find(el => el.children.includes(slider));
  return {
    slider,
    readout: row.children[1],
    status: all.find(el => el.tagName === 'SMALL'),
    start: all.find(el => el.tagName === 'BUTTON' && el.textContent === 'Charge now'),
    sent,
  };
}

const onePhase = {
  id: 'garage', phases: 1, voltage_v: 230, plugged_in: true,
  min_charge_w: 1380, max_charge_w: 7360, allowed_steps_w: [0, 1380, 2300, 3680],
};

test('a one-phase charger tops out at the top of its step table', () => {
  const ui = build(onePhase);
  assert.equal(ui.slider.min, '6');
  assert.equal(ui.slider.max, '16');
  assert.equal(ui.readout.textContent, '16 A · 3.7 kW');
  assert.match(ui.status.textContent, /^Starts at up to 16 A · 3\.7 kW\./);
  ui.start.fire('click');
  assert.deepEqual(ui.sent, [{ power_w: 3680, hold_s: 0, phase_mode: '1p' }]);
});

test('a hold above the table reads as the top Core can order', () => {
  const ui = build({ ...onePhase, manual_active: true, manual_charge_w: 7360 });
  assert.equal(ui.slider.value, '16');
  assert.equal(ui.readout.textContent, '16 A · 3.7 kW');
});

test('the top is the highest step within max_charge_w', () => {
  const ui = build({
    id: 'garage', phases: 3, voltage_v: 230, plugged_in: true,
    min_charge_w: 4140, max_charge_w: 7360, allowed_steps_w: [0, 4140, 6900, 11040],
  });
  assert.equal(ui.slider.max, '10');
  assert.equal(ui.readout.textContent, '10 A · 6.9 kW');
});

test('without a table the top is max_charge_w, as before', () => {
  const ui = build({ id: 'garage', phases: 3, voltage_v: 230, min_charge_w: 4140, max_charge_w: 11040 });
  assert.equal(ui.slider.min, '6');
  assert.equal(ui.slider.max, '16');
  assert.equal(ui.readout.textContent, '16 A · 11.0 kW');
});

// A top step off whole amps: 10,400 W at 3 × 230 V is 15.07 A. Sending
// 15 A = 10,350 W made Core floor the hold to 9,700 W, so the top step
// could not be ordered (Codex, #1808).
const offWholeAmps = {
  id: 'garage', phases: 3, voltage_v: 230, plugged_in: true,
  min_charge_w: 4140, max_charge_w: 11000, allowed_steps_w: [0, 4140, 6900, 9700, 10400],
};

test('the top orders a step that is not on whole amps', () => {
  const ui = build(offWholeAmps);
  assert.equal(ui.slider.max, '15');
  assert.equal(ui.readout.textContent, '15 A · 10.4 kW');
  assert.match(ui.status.textContent, /^Starts at up to 15 A · 10\.4 kW\./);
  ui.start.fire('click');
  assert.deepEqual(ui.sent, [{ power_w: 10400, hold_s: 0, phase_mode: '3p' }]);
});

test('below the top the slider still sends its amps', () => {
  // 14 A = 9,660 W lies between steps; Core floors it to 6,900 W, as before.
  const ui = build(offWholeAmps);
  ui.slider.value = '14';
  ui.slider.fire('input');
  assert.equal(ui.readout.textContent, '14 A · 9.7 kW');
  ui.start.fire('click');
  assert.deepEqual(ui.sent, [{ power_w: 9660, hold_s: 0, phase_mode: '3p' }]);
});

test('a hold at the top step reads as the top', () => {
  const ui = build({ ...offWholeAmps, manual_active: true, manual_charge_w: 10400 });
  assert.equal(ui.slider.value, '15');
  assert.equal(ui.readout.textContent, '15 A · 10.4 kW');
});

// srcfl/ftw#1861: a three-phase loadpoint with a maximum of 3,800 W, below
// 6 A on each phase. Core orders 4,140 W, the lowest three-phase level; the
// slider read "7 A · 3.8 kW".
test('a top below 6 A a phase reads and orders 6 A, as Core does', () => {
  const ui = build({
    id: 'rav4', phases: 3, voltage_v: 230, plugged_in: true,
    min_charge_w: 3800, max_charge_w: 3800, allowed_steps_w: [0, 3800],
  });
  assert.equal(ui.readout.textContent, '6 A · 4.1 kW');
  assert.match(ui.status.textContent, /^Starts at up to 6 A · 4\.1 kW\./);
  ui.start.fire('click');
  assert.deepEqual(ui.sent, [{ power_w: 4140, hold_s: 0, phase_mode: '3p' }]);
});
