import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const view = source.slice(
  source.indexOf('function buildEvPlanView'),
  source.indexOf('function buildEvCapacityView'),
);

// The plug-in moment (#1059): the modal shows what the box will do and
// lets the car's charge level be corrected without a button.

test('the plan view draws the planned windows on a 24 h track', () => {
  assert.match(view, /lpNow\.plan_windows/);
  assert.match(view, /EV_PLAN_HORIZON_MS/);
  // Every window is placed by wall clock and names its energy.
  assert.match(view, /w\.start_ms/);
  assert.match(view, /w\.wh \/ 1000/);
  // A manual hold is explained instead of drawn as a plan.
  assert.match(view, /Manual charge is selected/);
});

test('the charge-level slider writes on release, with no button', () => {
  assert.match(view, /slider\.addEventListener\("change"/);
  assert.match(view, /\/soc"/);
  assert.doesNotMatch(view, /Set current charge/);
  assert.doesNotMatch(view, /createElement\("button"\)/);
  // The refetch right after the write is what moves the plan on screen.
  assert.match(view, /Charge level saved:/);
  assert.match(view, /refreshEvModalAfterWrite\(\)/);
  // Polls do not snap the slider while the operator holds it.
  assert.match(view, /operatorHolds\(\)/);
});

test('the battery estimate stays separate from goal controls', () => {
  assert.doesNotMatch(source, /buildSoCSection/);
  const tabs = source.slice(
    source.indexOf('function buildEvControls'),
    source.indexOf('function utcMinsToLocalHHMM'),
  );
  assert.doesNotMatch(tabs, /Car is at|Car\'s current charge/);
});

test('the plan view is mounted once per loadpoint and updated on polls', () => {
  assert.match(source, /evPlanEl = buildEvPlanView\(matched, d\)/);
  assert.match(source, /evPlanLpId !== matched\.id/);
  assert.match(source, /evModalBody\.insertBefore\(evPlanEl\.el, statusTableEl\)/);
});
