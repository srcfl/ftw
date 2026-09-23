import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

// Manual charge runs until the car is full. The SoC estimate is a guess
// on chargers that cannot read the car, so gating Start on it produced
// a button that released itself on the next tick whenever the guess
// already sat at the schedule target (#1002 follow-up).

test('Start installs a hold without a SoC release', () => {
  const manual = source.slice(
    source.indexOf('function buildManualChargeSection'),
    source.indexOf('function sliderHeader'),
  );
  // The manual tab never sends release_at_soc_pct and never derives a
  // target from the schedule.
  assert.doesNotMatch(manual, /release_at_soc_pct/);
  assert.doesNotMatch(manual, /lp\.schedule/);
  // The button names its contract without a percentage.
  assert.match(manual, /startBtn\.textContent = "Charge now";/);
  assert.match(manual, /slider\.addEventListener\("change"/);
  assert.match(manual, /if \(lastLp\.manual_active\) requestCharge\(\)/);
});

test('an API-installed release target is still explained when active', () => {
  // A hold with release_at_soc_pct can still arrive through the API;
  // the manual tab and the plan strip keep saying where it stops.
  assert.match(source, /Math\.round\(lp\.manual_release_soc \* 100\)/);
  assert.match(source, /Returns to the plan at the estimated/);
});
