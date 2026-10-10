import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./energy-history.js', import.meta.url), 'utf8');

test('EV costs ignore charging outside the period and days without charging', () => {
  const calculatorSource = source.slice(source.indexOf('  function calculateEVChargeCost('),
    source.indexOf('  function renderEVCharging('));
  const calculate = vm.runInNewContext('(' + calculatorSource.trim() + ')');
  const point = (start, wh, quality = 'measured') => ({
    flow: 'vehicle_charge', bucket_start_ms: start, bucket_len_ms: 1800000,
    energy_wh: wh, quality,
  });
  const prices = [{ slot_ts_ms: 1800000, slot_len_min: 30, total_ore_kwh: 200 }];
  const result = calculate([point(0, 1000), point(1800000, 1500), point(3600000, 0, 'gap')],
    prices, 1800000, 5400000);
  assert.equal(result.energyWh, 1500);
  assert.equal(result.costMinor, 300);
  assert.equal(result.incomplete, false);
  const idle = calculate([point(0, 1000), point(1800000, 0, 'gap')], [], 1800000, 3600000);
  assert.equal(idle.energyWh, 0);
  assert.equal(idle.costMinor, 0);
  assert.equal(idle.incomplete, false);
  assert.equal(calculate([point(1800000, 1500)], [], 1800000, 3600000).incomplete, true);
});

test('History leads with a daily energy story and keeps raw data optional', () => {
  const history = html.match(/<main id="view-history"[\s\S]*?<main id="view-more"/)?.[0] || '';
  for (const id of [
    'energy-history-period-title', 'energy-history-range', 'energy-history-refresh',
    'energy-history-chart', 'energy-history-summary', 'energy-history-ev-cost', 'energy-history-insight',
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
  assert.match(source, /calculateEVChargeCost/);
  assert.match(source, /total_ore_kwh/);
  assert.match(html, /Based on FTW’s configured spot price, network tariff and VAT/);
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
    'energy-history-ev-cost',
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
  let evHistoryRequests = 0;
  let priceRequests = 0;
  let expectedChargeWh = 0;
  let expectedChargeOre = 0;
  let omitSecondPrice = false;
  const window = { addEventListener() {}, devicePixelRatio: 1 };

  vm.runInNewContext(source, {
    Array, Date, Intl, Map, Math, Number, Object, Promise, Set, String, URLSearchParams,
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
      if (path.startsWith('/api/energy/history?')) {
        const params = new URLSearchParams(path.split('?')[1]);
        if (params.get('bucket') === '30m') {
          evHistoryRequests += 1;
          const since = Number(params.get('since'));
          const bucket = 1800000;
          const today = new Date();
          today.setHours(0, 0, 0, 0);
          const first = Math.floor(today.getTime() / bucket) * bucket;
          const second = first + bucket;
          const chargePoints = [first, second].map((start) => ({
            bucket_start_ms: start, bucket_len_ms: bucket, flow: 'vehicle_charge',
            energy_wh: 1500, quality: 'measured', source: 'hardware_counter', provenance: 'counter',
          }));
          expectedChargeWh = 3000;
          expectedChargeOre = 450;
          return { ok: true, json: async () => ({ points: chargePoints, truncated: false }) };
        }
        ledgerRequests += 1;
        return { ok: true, json: async () => ({ points }) };
      }
      if (path.startsWith('/api/prices?')) {
        priceRequests += 1;
        const params = new URLSearchParams(path.split('?')[1]);
        // The cost assertion is supplied with slots matching the two readings
        // by using their query-window relationship (both ranges are whole days).
        const today = new Date();
        today.setHours(0, 0, 0, 0);
        const first = Math.floor(today.getTime() / 1800000) * 1800000;
        return { ok: true, json: async () => ({
          enabled: true, currency: 'SEK', items: [
            { slot_ts_ms: first, slot_len_min: 15, total_ore_kwh: 100 },
            { slot_ts_ms: first + 900000, slot_len_min: 15, total_ore_kwh: 200 },
            { slot_ts_ms: first + 1800000, slot_len_min: 15, total_ore_kwh: 100 },
            { slot_ts_ms: first + 2700000, slot_len_min: 15, total_ore_kwh: 200 },
          ].filter((_, index) => !(omitSecondPrice && index === 1))
        }) };
      }
      return { ok: false, json: async () => ({}) };
    },
    getComputedStyle: () => ({ getPropertyValue: () => '#94a3b8' }),
    window,
  });

  window.ftwEnergyHistoryLoad();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(dailyRequests, 1);
  assert.equal(ledgerRequests, 0);
  assert.equal(evHistoryRequests, 1);
  assert.equal(priceRequests, 1);
  assert.match(elements.get('energy-history-ev-cost').innerHTML, /Today/);
  assert.match(elements.get('energy-history-ev-cost').innerHTML, /This week/);
  assert.match(elements.get('energy-history-ev-cost').innerHTML, /This month/);
  assert.match(elements.get('energy-history-ev-cost').innerHTML, new RegExp(
    new Intl.NumberFormat(undefined, { minimumFractionDigits: expectedChargeWh / 1000 < 10 ? 1 : 0,
      maximumFractionDigits: expectedChargeWh / 1000 < 10 ? 1 : 0 }).format(expectedChargeWh / 1000) + ' kWh'
  ));
  assert.match(elements.get('energy-history-ev-cost').innerHTML, new RegExp(
    new Intl.NumberFormat(undefined, { style: 'currency', currency: 'SEK', maximumFractionDigits: 2 })
      .format(expectedChargeOre / 100).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  ));
  omitSecondPrice = true;
  window.ftwEnergyHistoryLoad();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(elements.get('energy-history-ev-cost').innerHTML, /Partial cost: charger or price data has gaps/);
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
