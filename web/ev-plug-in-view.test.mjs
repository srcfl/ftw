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


test('pending SoC write does not claim saved state or require re-entry', () => {
  const note = new Function(source.slice(source.indexOf('function sourceNote'), source.indexOf('var socPending')) + '; return sourceNote;')();
  const lp = { soc_source: 'inferred', current_soc: .8 };
  assert.match(note({ ...lp, soc_retention: 'pending' }), /Saving this level/);
  assert.doesNotMatch(note({ ...lp, soc_retention: 'pending' }), /must be entered again|could not be saved|keeps this level/);
  assert.match(note({ ...lp, soc_retention: 'session' }), /keeps this level/);
  assert.match(note({ ...lp, soc_retention: 'error' }), /could not be saved/);
});
