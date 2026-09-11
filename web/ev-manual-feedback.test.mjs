import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const manual = source.slice(
  source.indexOf('function buildManualChargeSection'),
  source.indexOf('function sliderHeader'),
);
const statusText = source.slice(
  source.indexOf('function manualStatusText'),
  source.indexOf('function renderEvPlanStatus'),
);

// After Charge now the operator must always know what is happening (#1002).
// Field report 2026-09-05: the tab said "Charging at 16 A" a tenth of a
// second after the click, the Easee cloud takes 5–15 s to act, nothing on
// screen moved, and the operator removed the charger to charge by hand.

test('the status line follows the charger through every state', () => {
  for (const state of ['sent', 'accepted', 'charging', 'not_drawing', 'stalled', 'limited', 'unavailable', 'pausing', 'paused']) {
    assert.match(statusText, new RegExp(`case "${state}":`));
  }
  assert.match(statusText, /Waiting for the charger/);
  assert.match(statusText, /Waiting for the car to start drawing/);
  assert.match(statusText, /but the car is not drawing/);
  assert.match(statusText, /The charger has not acted on/);
  assert.match(statusText, /Main fuse limits this charge/);
  // The charger's own words are part of the sentence.
  assert.match(statusText, /Charger reports: " \+ m\.charger_reason/);
  // Elapsed time comes from the box's since_ms, not from click time.
  assert.match(statusText, /m\.since_ms/);
});

test('the manual controls follow every poll', () => {
  assert.match(manual, /return \{ el: box, update: update \};/);
  assert.match(source, /manual\.update\(nextLp, d\);/);
  assert.match(source, /evControlsEl\.update\(matched, d\);/);
});

test("a refused Start reads as a failure with the server's reason", () => {
  assert.match(manual, /if \(r\.ok\) return r;/);
  assert.match(manual, /\.then\(failOn\)/);
  assert.match(manual, /"Request not confirmed: " \+ \(\(e && e\.message\) \|\| "try again"\)/);
  assert.doesNotMatch(manual, /"Charging at " \+ a \+ " A until the car is full/);
});

test('the plan strip uses the same sentence while a manual charge runs', () => {
  const strip = source.slice(
    source.indexOf('function renderEvPlanStatus'),
    source.indexOf('// EV modal sub-elements held across refreshes'),
  );
  assert.match(strip, /manualStatusText\(lp, d\)/);
});

const describeManual = new Function('formatW', 'evFmtElapsed', statusText + '; return manualStatusText;')(
  w => `${w} W`, ms => `${Math.floor(ms / 1000)} s`,
);
const lp = { manual_active: true, current_power_w: 0, manual: { active: true, requested_a: 16, commanded_a: 16 } };
test('an accepted limit at zero watts never claims that the car is charging', () => {
  const words = describeManual({ ...lp, manual: { ...lp.manual, state: 'accepted' } });
  assert.match(words, /Charger reports a 16 A limit/);
  assert.match(words, /Waiting for the car/);
  assert.doesNotMatch(words, /Charging at/);
});
test('a stale report overrides previously positive charging power', () => {
  const words = describeManual({ ...lp, current_power_w: 11000, manual: { ...lp.manual, state: 'unavailable' } });
  assert.match(words, /out of date/);
  assert.doesNotMatch(words, /Charging at/);
});
test('charging reported without a power reading stays explicit', () => {
  const words = describeManual({ ...lp, manual: { ...lp.manual, state: 'charging' } });
  assert.match(words, /Waiting for a power reading/);
  assert.doesNotMatch(words, /Charging at 0/);
});
test('an estimated release target remains visible while waiting', () => {
  const words = describeManual({ ...lp, manual_release_soc: 0.8, manual: { ...lp.manual, state: 'sent' } });
  assert.match(words, /estimated 80 % target/);
  assert.doesNotMatch(words, /Charging at/);
});


test('pause status waits for the charger before claiming it stopped', () => {
  const waiting = describeManual({ ...lp, manual: { ...lp.manual, state: 'pausing', requested_a: 0, requested_w: 0 }, current_power_w: 11000 });
  assert.match(waiting, /Waiting for the charger to stop/);
  assert.doesNotMatch(waiting, /Paused by you/);
  const paused = describeManual({ ...lp, manual: { ...lp.manual, state: 'paused', requested_a: 0, requested_w: 0 } });
  assert.match(paused, /Paused by you/);
  assert.match(paused, /until you resume the plan/);
});


test('a charger limit does not blame the main fuse', () => {
  const words = describeManual({ ...lp, manual: { ...lp.manual, state: 'limited', limit_reason: 'charger_limit', commanded_a: 10 } });
  assert.match(words, /The charger limits this request to 10 A/);
  assert.doesNotMatch(words, /Main fuse/);
});


test('an unconfirmed charger or connection asks for a choice without claiming a stop', () => {
  const words = describeManual({ manual_restore_unconfirmed: true, manual_active: false });
  assert.match(words, /Confirm how to continue charging/);
  assert.match(words, /could not confirm the charger or connection/);
  assert.doesNotMatch(words, /after restart|[Pp]aused|stopped/);
  assert.match(manual, /Pause charging to request a stop/);
  assert.doesNotMatch(words, /Paused by you/);
});
test('a lower requested current keeps actual old power visible until it arrives', () => {
  const words = describeManual({ ...lp, current_power_w: 11000, manual: { ...lp.manual, state: 'sent', requested_a: 6 } });
  assert.match(words, /confirm the new limit/);
  assert.match(words, /Still charging at 11000 W/);
});


test('failed hold persistence stays visible until the box reports recovery', () => {
  const strip = source.slice(source.indexOf('function renderEvPlanStatus'), source.indexOf('// Keep controls mounted while polling'));
  const render = new Function('document', 'manualStatusText', strip + '; return renderEvPlanStatus;')(
    { createElement: () => ({ style: {} }) }, describeManual,
  );
  const paused = { ...lp, plugged_in: true, manual_save_error: true, manual: { ...lp.manual, state: 'paused', requested_a: 0, requested_w: 0 } };
  assert.match(render(paused).textContent, /Paused by you/);
  assert.match(render(paused).textContent, /This choice is active now, but could not be saved for restart. FTW is retrying/);
  assert.doesNotMatch(render({ ...paused, manual_save_error: false }).textContent, /could not be saved/);
  // Returning to the plan also changes the persisted choice: a failed clear
  // must remain visible even though there is no active hold anymore.
  assert.match(render({ ...paused, manual_active: false }).textContent, /could not be saved for restart/);
});
