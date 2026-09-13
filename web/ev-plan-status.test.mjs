import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

// The EV modal's plan-status strip (#1002): one sentence on why the
// charger is (not) charging and when it will. These assertions pin the
// user-facing decision branches so a refactor can't silently drop one.

test('plan-status strip renders every visibility state', () => {
  // Planned window with a start/end clock and the planned energy.
  assert.match(source, /Charging planned " \+ evFmtClock\(lp\.plan_next_start_ms\)/);
  assert.match(source, /plan_total_wh \/ 1000/);
  // Charger offering power the car does not take, with the charger's
  // own reason when the driver reports one.
  assert.match(source, /Waiting for the car to draw power/);
  assert.match(source, /reason_no_current_label/);
  // The silent grid-plan deferral is named instead of looking like a
  // PV-only mode nobody chose.
  assert.match(source, /Waiting for tomorrow's electricity prices/);
  assert.match(source, /grid_deferred/);
  // Manual hold names its cost: the plan is off until the car is full,
  // Stop or unplug.
  assert.match(source, /Continues until the car stops drawing/);
  // The do-nothing default is called out with the three ways out.
  assert.match(source, /No charging plan yet/);
});

test('plan-status strip is the headline of the plan view', () => {
  // The sentence is rendered inside buildEvPlanView's update(), which
  // runs on every poll like the status table.
  assert.match(source, /var fresh = renderEvPlanStatus\(lpNow, dNow\)/);
  assert.match(source, /evPlanEl\.update\(matched, d\)/);
  // And reset when the modal short-circuits to "no charger".
  assert.match(source, /statusTableEl = null;\s*\n\s*evPlanEl = null;/);
});

const planStatus = new Function('document', 'manualStatusText', 'formatW', 'evFmtClock',
  source.slice(source.indexOf('function renderEvPlanStatus'), source.indexOf('// Keep controls mounted while polling')) + ';return renderEvPlanStatus;'
)(
  { createElement: () => ({ style: {}, textContent: '' }) },
  () => 'Paused by you.', w => `${w} W`, () => '07:00',
);

test('pending and failed plans do not pretend that charging windows are ready', () => {
  const lp = { plugged_in: true, schedule: { soc: .8 }, current_power_w: 0 };
  assert.match(planStatus({ ...lp, plan_pending: true, plan_outdated: true }, {}).textContent, /Updating the charging plan/);
  const failed = planStatus({ ...lp, plan_pending: false, plan_outdated: true }, {}).textContent;
  assert.match(failed, /Charging times are unavailable.*settings are saved/);
  assert.doesNotMatch(failed, /Updating|Charging planned/);
  assert.equal(planStatus({ ...lp, manual_active: true, plan_pending: true }, {}).textContent, 'Paused by you.');
});

test('reached goal explains a stopped charge and names the level source', () => {
  const lp = { plugged_in: true, charger: { available: true }, current_power_w: 0,
    commanded_known: true, commanded_w: 0, target_soc: .8, current_soc: .8014,
    soc_source: 'inferred', schedule: { soc: .8 } };
  const estimated = planStatus(lp, {}).textContent;
  assert.match(estimated, /Charge target reached \(80%\).*estimated by FTW/);
  assert.doesNotMatch(estimated, /No charge window|Choose Charge now/);
  assert.match(planStatus({ ...lp, soc_source: 'vehicle' }, {}).textContent, /reported by the car/);
  for (const change of [
    { soc_source: 'assumed' }, { current_soc: .79 }, { target_soc: null },
    { power_unavailable: true }, { charger: { known: true, available: false } },
    { current_power_w: 6900 }, { commanded_w: 6900 }, { commanded_known: false },
  ]) {
    assert.doesNotMatch(planStatus({ ...lp, ...change }, {}).textContent, /target reached/);
  }
});
