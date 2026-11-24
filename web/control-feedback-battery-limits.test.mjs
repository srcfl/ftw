import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {controlNumbers, controlStatus} from './control-feedback.js';

// Core's verdict when a battery gives less than ordered because that is what
// it can do now (go/internal/api/api_control_capability.go).
const row = extra => ({driver: 'sungrow', kind: 'battery', mode: 'planner_arbitrage', status: 'limited', severity: 'info',
  evidence: 'measured', readings_fresh: true, sent_w: 5000, requested_w: 5000, actual_w: 3600, ...extra});
const nov8 = new Date(2026, 10, 8, 2).getTime();

test('a learned grid import limit says what holds the battery and since when', () => {
  const shown = controlStatus(row({reason: 'learned_limit', limit_source: 'learned', limit_kind: 'grid_import',
    limit_w: 4316, limit_room_w: 3610, limit_seen: 14, limit_since_ms: nov8}));
  assert.equal(shown.title, 'At its usual limit');
  assert.equal(shown.text, 'The inverter takes at most 4.3 kW from the grid, which leaves 3.6 kW for the battery. FTW has seen this 14 times since 8 Nov.');
  assert.equal(shown.next, '');
  assert.equal(shown.tone, 'neutral');
});

test('a learned limit that replaced another names both', () => {
  const shown = controlStatus(row({reason: 'learned_limit', limit_kind: 'grid_import', limit_w: 3100,
    limit_seen: 3, limit_since_ms: new Date(2026, 10, 22, 2).getTime(), limit_earlier_w: 4316}));
  assert.equal(shown.text, 'The inverter takes at most 3.1 kW from the grid. FTW has seen this 3 times since 22 Nov. Earlier it was 4.3 kW.');
});

test('a learned current limit gives amps and the watts they make now', () => {
  const discharge = {sent_w: -5000, requested_w: -5000, actual_w: -4190, reason: 'learned_limit', limit_kind: 'battery_current',
    limit_a: 22.4, limit_w: 4189, limit_seen: 5, limit_since_ms: nov8};
  assert.equal(controlStatus(row(discharge)).text, 'The battery gives at most 22 A (4.2 kW now). FTW has seen this 5 times since 8 Nov.');
  assert.equal(controlStatus(row({...discharge, limit_w: undefined})).text, 'The battery gives at most 22 A. FTW has seen this 5 times since 8 Nov.');
  assert.equal(controlStatus(row({reason: 'learned_limit', limit_kind: 'battery_power', limit_w: 4200})).text, 'The battery takes at most 4.2 kW.');
  assert.equal(controlStatus(row({reason: 'learned_limit', limit_kind: 'grid_export', limit_w: 3000, sent_w: -5000})).text,
    'The inverter sends at most 3.0 kW to the grid.');
});

test('a limit the battery reports explains itself', () => {
  const shown = controlStatus(row({reason: 'battery_limit', limit_source: 'device', limit_kind: 'battery_reported',
    limit_w: 3200, sent_w: -5000, requested_w: -5000, actual_w: -3150}));
  assert.equal(shown.title, 'Limited by the battery');
  assert.equal(shown.text, 'The battery reports that it can give at most 3.2 kW now. FTW asked for 5.0 kW discharge.');
  assert.equal(shown.tone, 'neutral');
  assert.equal(controlStatus(row({reason: 'battery_limit', limit_w: 0, actual_w: 0})).text,
    'The battery reports that it cannot take power now. FTW asked for 5.0 kW charge.');
});

test('an inverter with no grid headroom left names the inverter', () => {
  const shown = controlStatus(row({reason: 'battery_limit', limit_source: 'device', limit_kind: 'inverter_import', limit_w: 0}));
  assert.equal(shown.title, 'Limited by the inverter');
  assert.equal(shown.text, 'The inverter reports that it can take no more from the grid now. FTW asked for 5.0 kW charge.');
  assert.equal(shown.next, '');
  assert.equal(shown.tone, 'neutral');
  const discharge = {sent_w: -5000, requested_w: -5000, actual_w: -3150, reason: 'battery_limit', limit_source: 'device', limit_kind: 'inverter_export'};
  assert.equal(controlStatus(row({...discharge, limit_w: 0})).text,
    'The inverter reports that it can send no more to the grid now. FTW asked for 5.0 kW discharge.');
  assert.equal(controlStatus(row({...discharge, limit_w: 60})).text,
    'The inverter reports that it can send only 60 W more to the grid now. FTW asked for 5.0 kW discharge.');
  // The battery's own report keeps the battery's words.
  assert.equal(controlStatus(row({reason: 'battery_limit', limit_kind: 'battery_reported', limit_w: 3600})).title, 'Limited by the battery');
});

// Core names each inverter limit it sets; each one has the inverter's words.
test('every inverter limit Core sets has words', () => {
  const go = readFileSync(new URL('../go/internal/api/api_control_capability.go', import.meta.url), 'utf8');
  const kinds = [...new Set([...go.matchAll(/"(inverter_[a-z]+)"/g)].map(m => m[1]))].sort();
  assert.deepEqual(kinds, ['inverter_export', 'inverter_import']);
  for (const limit_kind of kinds) {
    const shown = controlStatus(row({reason: 'battery_limit', limit_kind, limit_w: 0}));
    assert.equal(shown.title, 'Limited by the inverter', limit_kind);
    assert.match(shown.text, /^The inverter reports that it can (take|send) no more /, limit_kind);
  }
});

test('a battery on standby still asks for a look', () => {
  const shown = controlStatus(row({status: 'not_following', severity: 'warning', reason: 'device_standby', sent_w: -5000, actual_w: 0}));
  assert.equal(shown.title, 'On standby');
  assert.equal(shown.text, 'Asked for 5.0 kW discharge, but the battery reports that it is on standby or starting up.');
  assert.equal(shown.next, 'It usually starts again by itself. If it stays on standby, check the battery’s app or display.');
  assert.equal(shown.tone, 'warning');
});

// srcfl/ftw-webapp#144 found both; the box keeps the app's words.
test('a reported limit without a target says what FTW asked for once', () => {
  assert.equal(controlStatus(row({reason: 'battery_limit', sent_w: undefined, requested_w: undefined})).text,
    'The battery reports a limit below what FTW asked for.');
  assert.equal(controlStatus(row({reason: 'battery_limit'})).text, 'The battery reports a limit below the 5.0 kW charge FTW asked for.');
});

test('a battery current limit is the battery\'s, not a charger\'s', () => {
  const numbers = controlNumbers(row({reason: 'learned_limit', limit_kind: 'battery_current', limit_a: 22.4, sent_w: -5000}));
  assert.deepEqual(numbers.find(([label]) => label === 'Battery current limit'), ['Battery current limit', '22 A']);
  assert.ok(!numbers.some(([label]) => /Charger|car/.test(label)), JSON.stringify(numbers));
  const charger = controlNumbers({driver: 'easee', kind: 'ev', status: 'limited', reason: 'offered_current_lower', limit_a: 10});
  assert.deepEqual(charger.find(([, value]) => value === '10 A'), ['Charger lets the car take', '10 A']);
});

test('limit words never show codes or invent numbers', () => {
  assert.equal(controlStatus(row({reason: 'learned_limit', limit_kind: 'grid_import'})).text, 'The battery is at a limit it has shown before.');
  assert.equal(controlStatus(row({reason: 'battery_limit'})).text, 'The battery reports a limit below the 5.0 kW charge FTW asked for.');
  for (const extra of [{reason: 'learned_limit'}, {reason: 'learned_limit', limit_kind: 'grid_import'}, {reason: 'battery_limit'},
    {reason: 'learned_limit', limit_kind: 'battery_current', limit_seen: 4}, {reason: 'device_standby', sent_w: undefined},
    {reason: 'battery_limit', limit_kind: 'inverter_import'}, {reason: 'battery_limit', limit_kind: 'inverter_export', sent_w: undefined},
    {reason: 'battery_limit', sent_w: undefined}]) {
    const shown = controlStatus(row(extra));
    const text = [shown.title, shown.text, shown.next].join(' ');
    assert.doesNotMatch(text, /_|undefined|NaN|null|cannot describe|\bthe the\b/, JSON.stringify(extra));
  }
});
