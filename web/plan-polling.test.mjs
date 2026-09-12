import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';
import * as brief from './plan-brief.js';
import * as soc from './plan-soc.js';
import * as units from './components/price-units.js';
import * as prefs from './plan-prefs.js';

const source = readFileSync(new URL('./plan.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from "[^"]+";\n/gm, '');

function rigPlan() {
  const listeners = new Map();
  const intervals = new Map();
  const fetches = [];
  let timer = 0;
  const document = {
    hidden: false, readyState: 'complete',
    body: { appendChild() {} },
    createElement() { return { style: {} }; },
    getElementById() { return null; }, querySelectorAll() { return []; },
    addEventListener(name, fn) { listeners.set(name, fn); },
  };
  const sandbox = {
    ...brief, ...soc, ...units, ...prefs, document,
    window: { addEventListener() {}, dispatchEvent() {} },
    CustomEvent: class { constructor(name, options) { this.type = name; this.detail = options.detail; } },
    fetch(path) {
      let resolve;
      const promise = new Promise(r => { resolve = r; });
      fetches.push({ path, resolve });
      return promise;
    },
    setInterval(fn, ms) { intervals.set(++timer, { fn, ms }); return timer; },
    clearInterval(id) { intervals.delete(id); },
    console,
  };
  vm.runInNewContext(source, sandbox);
  return {
    batches() { return fetches.filter(f => f.path === '/api/config').length; },
    setHidden(value) { document.hidden = value; listeners.get('visibilitychange')(); },
    poll() { for (const { fn, ms } of intervals.values()) if (ms === 30000) fn(); },
    async settle() {
      for (const f of fetches.splice(0)) f.resolve({ json: async () => ({}) });
      await new Promise(resolve => setImmediate(resolve));
    },
  };
}

test('plan resume waits for the current batch then catches up once', async () => {
  const plan = rigPlan();
  assert.equal(plan.batches(), 1);
  plan.setHidden(true);
  plan.setHidden(false);
  plan.poll();
  plan.poll();
  assert.equal(plan.batches(), 1, 'resume must not start an overlapping six-request batch');
  await plan.settle();
  assert.equal(plan.batches(), 1, 'one catch-up starts after the first batch settles');
  await plan.settle();
  assert.equal(plan.batches(), 0, 'catch-up must stop once current');
});

test('plan drops its queued refresh if hidden again before the response', async () => {
  const plan = rigPlan();
  plan.setHidden(true);
  plan.setHidden(false);
  plan.setHidden(true);
  await plan.settle();
  assert.equal(plan.batches(), 0);
  plan.setHidden(false);
  assert.equal(plan.batches(), 1, 'the next visible transition still refreshes');
  await plan.settle();
});
