import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./energy-history.js', import.meta.url), 'utf8');

test('History leads with a daily energy story and keeps raw data optional', () => {
  const history = html.match(/<main id="view-history"[\s\S]*?<main id="view-more"/)?.[0] || '';
  for (const id of [
    'energy-history-period-title', 'energy-history-range', 'energy-history-refresh',
    'energy-history-chart', 'energy-history-summary', 'energy-history-insight',
    'energy-history-details', 'energy-history-asset', 'energy-history-csv',
    'energy-history-quality', 'energy-history-detail-meta', 'energy-history-rows',
    'energy-history-prev', 'energy-history-page', 'energy-history-next',
  ]) {
    assert.equal((history.match(new RegExp(`id="${id}"`, 'g')) || []).length, 1, id);
  }
  assert.match(history, /Your energy, day by day/);
  assert.match(history, /Used at home|energy-history-summary/);
  assert.match(history, /Devices, data quality and export/);
  assert.match(history, /<th>Quality<\/th>/);
  assert.doesNotMatch(history, /<details[^>]+open/);
  assert.equal((html.match(/energy-history\.js/g) || []).length, 1);
});

test('main History uses daily totals while raw ledger stays a troubleshooting view', () => {
  assert.match(source, /\/api\/energy\/daily\?days=/);
  assert.match(source, /Used at home/);
  assert.match(source, /Made by solar/);
  assert.match(source, /Bought from grid/);
  assert.match(source, /Sent to grid/);
  assert.match(source, /point\.quality === 'invalid'/);
  assert.match(source, /impossible counter reading/);
  assert.match(source, /tablePageSize = 50/);
  assert.match(source, /window\.ftwEnergyHistoryLoad = load/);
});

test('raw energy rows load only when opened, flag rejected data and page locally', async () => {
  const listeners = new Map();
  const context2d = {
    beginPath() {}, clearRect() {}, fillRect() {}, fillText() {},
    lineTo() {}, moveTo() {}, scale() {}, stroke() {},
  };
  const classList = () => ({ toggle() {}, add() {}, remove() {} });
  const element = (values = {}) => ({
    children: [],
    classList: classList(),
    className: '',
    clientWidth: 800,
    disabled: false,
    href: '',
    innerHTML: '',
    open: false,
    textContent: '',
    value: '',
    addEventListener(name, handler) { listeners.set(values.id + ':' + name, handler); },
    appendChild(child) { this.children.push(child); },
    click() { listeners.get(values.id + ':click')?.(); },
    getContext() { return context2d; },
    ...values,
  });
  const ids = [
    'energy-history-period-title', 'energy-history-range', 'energy-history-asset',
    'energy-history-refresh', 'energy-history-csv', 'energy-history-summary',
    'energy-history-insight', 'energy-history-details', 'energy-history-quality',
    'energy-history-rows', 'energy-history-page', 'energy-history-prev',
    'energy-history-next', 'energy-history-detail-meta', 'energy-history-chart',
    'energy-history-legend',
  ];
  const elements = new Map(ids.map((id) => [id, element({ id })]));
  const points = Array.from({ length: 138 }, (_, index) => ({
    bucket_start_ms: 1_800_000_000_000 + index * 3_600_000,
    bucket_len_ms: 3_600_000,
    flow: 'grid_import',
    energy_wh: index === 0 ? 25_000_000 : index,
    quality: index === 0 ? 'invalid' : 'measured',
    source: 'hardware_counter',
    provenance: index === 0 ? 'implausible_energy' : 'counter',
  }));
  const days = [{
    day: '2026-07-24', import_wh: 4000, export_wh: 1000,
    pv_wh: 9000, load_wh: 12000, bat_charged_wh: 2000, bat_discharged_wh: 1500,
  }];
  let dailyRequests = 0;
  let ledgerRequests = 0;
  const window = { addEventListener() {}, devicePixelRatio: 1 };

  vm.runInNewContext(source, {
    Array, Date, Map, Math, Number, Object, Promise, Set, String, URLSearchParams,
    document: {
      createElement() { return element({ id: 'option' }); },
      documentElement: {},
      getElementById(id) { return elements.get(id) || null; },
      querySelectorAll() { return []; },
    },
    fetch: async (path) => {
      if (path.startsWith('/api/energy/daily')) {
        dailyRequests += 1;
        return { ok: true, json: async () => ({ days }) };
      }
      if (path === '/api/energy/assets') {
        return { ok: true, json: async () => ({ assets: [] }) };
      }
      ledgerRequests += 1;
      return { ok: true, json: async () => ({ points }) };
    },
    getComputedStyle: () => ({ getPropertyValue: () => '#94a3b8' }),
    window,
  });

  window.ftwEnergyHistoryLoad();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(dailyRequests, 1);
  assert.equal(ledgerRequests, 0);
  assert.match(elements.get('energy-history-summary').innerHTML, /Used at home/);
  assert.match(elements.get('energy-history-summary').innerHTML, /12 kWh/);

  const details = elements.get('energy-history-details');
  details.open = true;
  listeners.get('energy-history-details:toggle')();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(ledgerRequests, 1);
  assert.match(elements.get('energy-history-quality').textContent, /1 impossible counter reading was rejected/);
  assert.equal((elements.get('energy-history-rows').innerHTML.match(/<tr>/g) || []).length, 50);
  assert.equal(elements.get('energy-history-page').textContent, 'Page 1 of 3');
  assert.equal(elements.get('energy-history-prev').disabled, true);
  assert.equal(elements.get('energy-history-next').disabled, false);

  elements.get('energy-history-next').click();
  assert.equal(elements.get('energy-history-page').textContent, 'Page 2 of 3');
  assert.equal((elements.get('energy-history-rows').innerHTML.match(/<tr>/g) || []).length, 50);
  assert.equal(ledgerRequests, 1);
});
