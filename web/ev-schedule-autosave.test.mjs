import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const sched = source.slice(
  source.indexOf('function buildScheduleSection'),
  source.indexOf('function buildEvControls'),
);

// The goal editor is direct manipulation (#1065): every control writes
// when it changes and the plan view above redraws. No Save button.

test('every schedule control saves on change', () => {
  assert.doesNotMatch(source, /Set schedule|Update schedule/);
  assert.match(sched, /targetSlider\.addEventListener\("change", scheduleSave\)/);
  assert.match(sched, /timeInp\.addEventListener\("change", scheduleSave\)/);
  assert.match(sched, /recCb\.addEventListener\("change"/);
  assert.match(sched, /surCb\.addEventListener\("change"/);
  assert.match(sched, /unlockWrap\.input\.addEventListener\("change", scheduleSave\)/);
  // Debounced, sequence-guarded, and followed by a refetch so the plan
  // view above moves with the schedule.
  assert.match(sched, /setTimeout\(function \(\) \{ saveTimer = null; doSave\(\); \}, 400\)/);
  assert.match(sched, /if \(seq !== saveSeq\) return;/);
  assert.match(sched, /Schedule saved\./);
  assert.match(sched, /refreshEvModalAfterWrite\(\)/);
});

test('weekday chips speak the wire: bit 0 = Monday, all seven = zero', () => {
  assert.match(sched, /\["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"\]/);
  assert.match(sched, /days: days === 0x7f \? 0 : days/);
  assert.match(sched, /Pick at least one day\./);
  assert.match(sched, /aria-pressed/);
});

test('removing the schedule is the one explicit action', () => {
  assert.match(sched, /mkBtn\("Remove schedule"\)/);
  assert.match(sched, /schedule: null/);
});
