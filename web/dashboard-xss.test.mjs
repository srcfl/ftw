import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const app = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const plan = readFileSync(new URL('./plan.js', import.meta.url), 'utf8');
const pv = readFileSync(new URL('./components/ftw-pv-control.js', import.meta.url), 'utf8');

function sliceMethod(source, name, next) {
  const start = source.indexOf(name + '(');
  assert.ok(start >= 0, name + ' must exist');
  const end = next ? source.indexOf(next + '(', start + 1) : source.length;
  assert.ok(end > start, name + ' must be followed by ' + next);
  return source.slice(start, end);
}

test('status-bar driver names go through escHtml', () => {
  const bar = app.slice(app.indexOf('Status bar — driver health summary'));
  const html = bar.slice(0, bar.indexOf('if (sbVersion'));
  assert.match(html, /escHtml\(n\)/);
  assert.doesNotMatch(html, /" " \+ n \+/);
});

test('plan summary and tooltip escape planner reason text', () => {
  assert.match(plan, /function escapeHTML/);
  assert.match(plan, /escapeHTML\(state\.planMeta\.last_replan_reason/);
  assert.match(plan, /escapeHTML\(a\.reason\)/);
  assert.doesNotMatch(plan, /Reason: \$\{state\.planMeta\.last_replan_reason/);
  assert.doesNotMatch(plan, /tip-reason\}>\$\{a\.reason\}/);
});

test('PV curtail options are DOM nodes, not innerHTML of driver names', () => {
  const fn = sliceMethod(pv, '_renderDriverOptions', '_renderActive');
  assert.match(fn, /document\.createElement\("option"\)/);
  assert.match(fn, /opt\.textContent/);
  assert.match(fn, /opt\.value/);
  assert.doesNotMatch(fn, /sel\.innerHTML/);
  assert.doesNotMatch(fn, /<option value="\$\{/);
});
