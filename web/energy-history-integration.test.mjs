import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./energy-history.js', import.meta.url), 'utf8');

test('History contains one complete energy ledger surface', () => {
  const history = html.match(/<main id="view-history"[\s\S]*?<main id="view-more"/)?.[0] || '';
  for (const id of [
    'energy-history-asset', 'energy-history-range', 'energy-history-refresh',
    'energy-history-csv', 'energy-history-chart', 'energy-history-summary',
    'energy-history-details', 'energy-history-detail-meta', 'energy-history-rows',
    'energy-history-prev', 'energy-history-page', 'energy-history-next',
    'plan-history-details',
  ]) {
    assert.equal((history.match(new RegExp(`id="${id}"`, 'g')) || []).length, 1, id);
  }
  assert.equal((html.match(/id="history-numbers-details"/g) || []).length, 1);
  assert.match(history, /<th>Quality<\/th>/);
  assert.match(html, /<details id="history-numbers-details" class="history-disclosure history-numbers-disclosure">/);
  assert.match(history, /<details id="energy-history-details" class="history-disclosure">/);
  assert.match(history, /<details id="plan-history-details" class="history-disclosure history-plan-disclosure">/);
  assert.doesNotMatch(history, /<details[^>]+open/);
  assert.match(history, /Past plan decisions/);
  assert.equal((html.match(/energy-history\.js/g) || []).length, 1);
});

test('ledger chart uses returned buckets and ignores gaps and counter resets', () => {
  assert.match(source, /point\.bucket_start_ms/);
  assert.match(source, /point\.quality !== 'gap'/);
  assert.match(source, /point\.quality !== 'reset'/);
  assert.match(source, /window\.ftwEnergyHistoryLoad = load/);
  assert.match(source, /tablePageSize = 50/);
  assert.match(source, /tablePoints\.slice\(start, start \+ tablePageSize\)/);
  assert.match(source, /tablePage \+= 1/);
});

test('raw energy rows stay bounded and page without another request', async () => {
  const listeners = new Map();
  const context2d = {
    arc() {}, beginPath() {}, clearRect() {}, fill() {}, fillText() {},
    lineTo() {}, moveTo() {}, scale() {}, stroke() {},
  };
  const element = (values = {}) => ({
    children: [],
    clientWidth: 800,
    disabled: false,
    innerHTML: '',
    textContent: '',
    value: '',
    addEventListener(name, handler) { listeners.set(values.id + ':' + name, handler); },
    appendChild(child) { this.children.push(child); },
    click() { listeners.get(values.id + ':click')?.(); },
    getContext() { return context2d; },
    ...values,
  });
  const ids = [
    'energy-history-range', 'energy-history-asset', 'energy-history-refresh',
    'energy-history-csv', 'energy-history-summary', 'energy-history-rows',
    'energy-history-page', 'energy-history-prev', 'energy-history-next',
    'energy-history-detail-meta', 'energy-history-chart', 'energy-history-legend',
  ];
  const elements = new Map(ids.map((id) => [id, element({ id })]));
  elements.get('energy-history-range').value = '7d';
  const points = Array.from({ length: 138 }, (_, index) => ({
    bucket_start_ms: 1_800_000_000_000 + index * 3_600_000,
    flow: 'grid_import',
    energy_wh: index,
    quality: 'measured',
    source: 'hardware_counter',
    provenance: 'counter',
  }));
  let historyRequests = 0;
  const window = {
    addEventListener() {},
    devicePixelRatio: 1,
  };
  vm.runInNewContext(source, {
    Array, Date, Map, Math, Number, Object, Promise, Set, String, URLSearchParams,
    document: {
      createElement() { return element({ id: 'option' }); },
      documentElement: {},
      getElementById(id) { return elements.get(id) || null; },
    },
    fetch: async (path) => {
      if (path === '/api/energy/assets') {
        return { ok: true, json: async () => ({ assets: [] }) };
      }
      historyRequests += 1;
      return { ok: true, json: async () => ({ points }) };
    },
    getComputedStyle: () => ({ getPropertyValue: () => '#94a3b8' }),
    window,
  });

  window.ftwEnergyHistoryLoad();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal((elements.get('energy-history-rows').innerHTML.match(/<tr>/g) || []).length, 50);
  assert.equal(elements.get('energy-history-page').textContent, 'Page 1 of 3');
  assert.equal(elements.get('energy-history-prev').disabled, true);
  assert.equal(elements.get('energy-history-next').disabled, false);

  elements.get('energy-history-next').click();
  assert.equal(elements.get('energy-history-page').textContent, 'Page 2 of 3');
  assert.equal((elements.get('energy-history-rows').innerHTML.match(/<tr>/g) || []).length, 50);
  assert.equal(historyRequests, 1);
});
