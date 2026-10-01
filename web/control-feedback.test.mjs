import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {controlRows, controlStatus, controlReceipt, controlNumbers, controlCurve, controlForPlanet,
  controlSummary, withControlMarks} from './control-feedback.js';

const row = extra => ({driver: 'bat', kind: 'battery', mode: 'planner_arbitrage', status: 'following', reason: 'power_observed',
  severity: 'info', evidence: 'measured', readings_fresh: true, sent_w: -2250, requested_w: -2250, actual_w: -2249,
  readback_w: -2250, command_at_ms: 1, observed_at_ms: 2, site_confirmation: 'device_response_unconfirmed', ...extra});

// Every reason Core can classify must have words, or the owner sees a code.
test('every reason Core emits has a title and a sentence', () => {
  const go = readFileSync(new URL('../go/internal/api/api_control_feedback.go', import.meta.url), 'utf8');
  const body = go.slice(go.indexOf('func setControlStatus'), go.indexOf('f.Status, f.Severity = status, severity'));
  const reasons = [...body.matchAll(/"([a-z_]+)"/g)].map(m => m[1])
    .filter(r => !['waiting', 'info', 'following', 'limited', 'warning', 'not_following', 'alarm', 'no_contact', 'not_controlled'].includes(r));
  assert.ok(reasons.length > 30, `found only ${reasons.length} reasons`);
  for (const reason of reasons) {
    const words = controlStatus(row({reason, status: 'waiting'}));
    assert.doesNotMatch(words.text, /cannot describe/, reason);
    assert.ok(words.title && words.title !== 'Checking', reason);
  }
});

test('the default view never shows tier numbers or internal codes', () => {
  for (const [status, reason, severity] of [['following', 'power_observed', 'info'], ['waiting', 'waiting_response', 'info'],
    ['limited', 'battery_nearly_full', 'info'], ['not_following', 'power_below_target', 'warning'],
    ['no_contact', 'readings_lost', 'alarm'], ['not_controlled', 'observe_only', 'info']]) {
    const words = controlStatus(row({status, reason, severity, evidence: 'confirmed', confirmed_at_ms: 3, battery_soc: 0.97}));
    const shown = [words.title, words.text, words.proof, words.next].join(' ');
    assert.doesNotMatch(shown, /tier|_|undefined|NaN|null/i, `${reason}: ${shown}`);
  }
});

test('the answer follows Core: following, not following and lost control', () => {
  const following = controlStatus(row());
  assert.equal(following.title, 'Following FTW');
  assert.equal(following.text, 'Discharging 2.2 kW as planned.');
  assert.equal(following.tone, 'ok');
  const ignored = controlStatus(row({status: 'not_following', reason: 'no_power_response', severity: 'warning', sent_w: 3000, actual_w: 0}));
  assert.equal(ignored.title, 'Not following');
  assert.equal(ignored.text, 'Asked for 3.0 kW charge, but it delivers no power.');
  assert.match(ignored.next, /has not said why/);
  assert.equal(ignored.tone, 'warning');
  const lost = controlStatus(row({status: 'no_contact', reason: 'readings_lost', severity: 'alarm', evidence: 'accepted'}));
  assert.equal(lost.title, 'Lost control');
  assert.equal(lost.tone, 'alarm');
  assert.equal(controlStatus(row(), false).tone, 'stale');
});

test('expected limits stay calm and explain themselves', () => {
  const taper = controlStatus(row({status: 'limited', reason: 'battery_nearly_full', sent_w: 3000, actual_w: 900, battery_soc: 0.98}));
  assert.equal(taper.title, 'Battery nearly full');
  assert.equal(taper.text, 'Taking 900 W of 3.0 kW at 98%. A battery charges slower when it is nearly full.');
  assert.equal(taper.tone, 'neutral');
  const full = controlStatus(row({status: 'limited', reason: 'battery_full', sent_w: 0, actual_w: 0, battery_soc: 1, charge_resume_soc: 0.99}));
  assert.match(full.text, /paused charging at 100%\. Charging resumes at 99% or lower/);
  const car = controlStatus(row({kind: 'ev', reason: 'vehicle_complete', mode: 'plan'}));
  assert.equal(car.title, 'Car is full');
  const limit = controlStatus(row({kind: 'ev', status: 'limited', reason: 'device_limit', severity: 'warning', device_limit_a: 8, requested_a: 16}));
  assert.equal(limit.text, 'The charger’s own limit is 8 A, below the 16 A FTW asked for.');
});

test('the receipt walks from sent to confirmed without inventing proof', () => {
  const confirmed = controlReceipt(row({evidence: 'confirmed', confirmed_at_ms: 3, site_confirmation: 'confirmed', site_evidence: {device_change_w: -836}}));
  assert.deepEqual(confirmed.map(s => [s.step, s.state]), [['Sent', 'done'], ['Accepted', 'done'], ['Measured', 'done'], ['Confirmed', 'done']]);
  assert.match(confirmed[3].value, /grid meter matched a 836 W change/);
  const limited = controlReceipt(row({requested_w: -5000, mode: 'manual'}));
  assert.deepEqual(limited[0], {step: 'Asked', value: '5.0 kW discharge · manual', state: 'done'});
  const accepted = controlReceipt(row({evidence: 'accepted', status: 'waiting', reason: 'waiting_response'}));
  assert.deepEqual(accepted.map(s => s.state), ['done', 'done', 'wait', 'wait']);
  const stale = controlReceipt(row({evidence: 'accepted', readings_fresh: false, actual_w: null}));
  assert.equal(stale[2].state, 'fail');
  const failed = controlReceipt(row({evidence: 'none', reason: 'command_failed', status: 'no_contact'}));
  assert.equal(failed[1].state, 'fail');
  const noMeter = controlReceipt(row({site_confirmation: 'independent_source_unknown'}));
  assert.deepEqual([noMeter[3].value, noMeter[3].state], ['The grid meter is not a separate sensor', 'none']);
  assert.ok(controlReceipt(row(), false).slice(2).every(s => s.value === 'Not current'));
});

test('numbers keep signs and never invent zeros', () => {
  const values = Object.fromEntries(controlNumbers(row({site_evidence: {grid_before_w: -5663, grid_after_w: -6471,
    device_change_w: -836, grid_change_w: -808, other_change_w: -28, unexplained_change_w: 56, samples: 4, window_s: 15,
    unmeasured_flows: ['easee:ev'], max_skew_ms: 0}})));
  assert.equal(values['Grid before'], '5.7 kW export');
  assert.equal(values['Device change'], '−836 W');
  assert.equal(values['Unexplained change'], '+56 W');
  assert.equal(values['Left in the background'], 'easee (car charger)');
  const missing = Object.fromEntries(controlNumbers(row({site_evidence: {samples: 0}})));
  assert.equal(missing['Grid before'], undefined);
  assert.equal(missing['Largest time gap'], undefined);
  assert.deepEqual(controlNumbers(row(), false), []);
});

test('curve evidence expires with status and rejects invalid samples', () => {
  const trace = [0, 5000, 10000].map(at_ms => ({at_ms, device_change_w: -800, adjusted_site_change_w: -780}));
  assert.ok(controlCurve(row({site_evidence: {trace}})));
  assert.equal(controlCurve(row({site_evidence: {trace}}), false), null);
  assert.equal(controlCurve(row({site_evidence: {trace: trace.slice(0, 2)}})), null);
  assert.equal(controlCurve(row({site_evidence: {trace: [trace[0], trace[0], trace[1]]}})), null);
});

test('marks appear only when Core asks for attention', () => {
  const planets = [{name: 'bat', role: 'battery'}, {name: 'car', role: 'ev'}];
  const calm = withControlMarks(planets, [row(), row({driver: 'car', kind: 'ev', status: 'waiting', reason: 'not_connected'})]);
  assert.ok(calm.every(p => !p.controlMark));
  const marked = withControlMarks(planets, [row({status: 'not_following', reason: 'setpoint_changed', severity: 'warning'}),
    row({driver: 'car', kind: 'device', status: 'not_following', reason: 'device_fault', severity: 'alarm'})]);
  assert.deepEqual(marked.find(p => p.name === 'bat').controlMark, {tone: 'warning', label: 'Not following'});
  assert.deepEqual(marked.find(p => p.name === 'car').controlMark, {tone: 'alarm', label: 'Device fault'});
  assert.ok(withControlMarks(planets, [row({severity: 'alarm', status: 'no_contact', reason: 'readings_lost'})], false).every(p => !p.controlMark));
});

test('a silent device keeps its bubble without old watts', () => {
  const planets = withControlMarks([{name: 'a', role: 'battery', kw: 1}], [row({driver: 'a'}),
    row({driver: 'b', status: 'no_contact', reason: 'readings_lost', severity: 'alarm', readings_fresh: false, actual_w: null})]);
  const silent = planets.find(p => p.name === 'b');
  assert.equal(silent.placeholder, true);
  assert.equal(silent.kw, 0);
  assert.equal(silent.controlMark.tone, 'alarm');
  assert.equal(planets.find(p => p.name === 'a').kw, 1);
  const monitored = withControlMarks([{name: 'm', role: 'battery', kw: 2}], [row({driver: 'm', status: 'not_controlled', reason: 'observe_only', readings_fresh: false})]);
  assert.equal(monitored[0].kw, 2);
  assert.notEqual(monitored[0].clickable, true);
});

test('a sheet gets its device rows and combined bubbles keep every device', () => {
  const rows = [row(), row({driver: 'two'}), row({driver: 'car', kind: 'ev'}), row({driver: 'two', kind: 'device', reason: 'device_fault'})];
  assert.deepEqual(controlForPlanet(rows, {role: 'battery', name: 'two'}).map(r => r.kind), ['battery', 'device']);
  assert.equal(controlForPlanet(rows, {role: 'battery', id: 'agg-battery', name: '2×'}).length, 3);
  assert.equal(controlRows([{driver: 'x'}, null, row()]).length, 1);
});

test('the summary answers the question for the whole site', () => {
  assert.deepEqual(controlSummary([row()]), {title: 'Yes, FTW is in control', tone: 'ok'});
  assert.equal(controlSummary([row(), row({severity: 'warning'})]).title, '1 device needs a look');
  assert.equal(controlSummary([row({severity: 'alarm'}), row({severity: 'alarm'})]).title, '2 devices need attention now');
  assert.equal(controlSummary([row({status: 'not_controlled', reason: 'observe_only'})]).tone, 'neutral');
  assert.equal(controlSummary([row()], false).tone, 'stale');
});
