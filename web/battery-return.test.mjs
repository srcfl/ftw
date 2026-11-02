import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./battery-return.js', import.meta.url), 'utf8');

test('History offers the battery refund figure closed, with its hourly basis', () => {
  const history = html.match(/<main id="view-history"[\s\S]*?<main id="view-more"/)?.[0] || '';
  for (const id of [
    'battery-return-details', 'battery-return-meta', 'battery-return-year',
    'battery-return-csv', 'battery-return-result', 'battery-return-rows',
  ]) {
    assert.equal((history.match(new RegExp(`id="${id}"`, 'g')) || []).length, 1, id);
  }
  assert.match(history, /Energy tax refund for battery storage/);
  assert.match(history, /skatteverket\.se\/privat\/skatter\/skattpabransleochel\/aterbetalningavskattpaelefterbatterilagring/);
  assert.doesNotMatch(history, /<details id="battery-return-details"[^>]+open/);
  assert.equal((html.match(/battery-return\.js/g) || []).length, 1);
});

function harness(response) {
  const listeners = new Map();
  const element = (id) => ({
    id,
    children: [],
    className: '',
    href: '',
    open: false,
    textContent: '',
    value: '',
    addEventListener(name, handler) { listeners.set(id + ':' + name, handler); },
    appendChild(child) { this.children.push(child); },
  });
  const ids = [
    'battery-return-details', 'battery-return-year', 'battery-return-csv',
    'battery-return-result', 'battery-return-rows', 'battery-return-meta',
  ];
  const elements = new Map(ids.map((id) => [id, element(id)]));
  const requests = [];
  vm.runInNewContext(source, {
    Date, Intl, Math, Number, Object, Promise, String, encodeURIComponent,
    document: {
      createElement: (tag) => element(tag),
      createTextNode: (text) => ({ textContent: text }),
      getElementById: (id) => elements.get(id) || null,
    },
    fetch: async (path) => {
      requests.push(path);
      return { ok: true, json: async () => response };
    },
  });
  const text = (node) => node.textContent + (node.children || []).map(text).join('');
  return { elements, listeners, requests, text };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test('the figure loads when opened, for last year, rounded down to whole kWh', async () => {
  const thisYear = Number(new Intl.DateTimeFormat('en', { year: 'numeric', timeZone: 'Europe/Stockholm' }).format(new Date()));
  const months = Array.from({ length: 12 }, (_, i) => ({ month: i + 1, returned_wh: i === 0 ? 1999.9 : 0, hours: 744, complete_hours: 700 }));
  const { elements, listeners, requests, text } = harness({
    year: thisYear - 1, returned_wh: 123_999.9, hours: 8760, complete_hours: 8700, months,
    capacity_wh: 10_240, efficiency: 0.8973, problem: '', batteries: 1,
  });
  const select = elements.get('battery-return-year');
  assert.equal(select.value, String(thisYear - 1));
  assert.equal(select.children.length, 4);
  assert.equal(requests.length, 0);

  elements.get('battery-return-details').open = true;
  listeners.get('battery-return-details:toggle')();
  await tick();
  await tick();

  assert.deepEqual(requests, ['/api/energy/battery-return?year=' + (thisYear - 1)]);
  assert.equal(elements.get('battery-return-csv').href, '/api/energy/battery-return.csv?year=' + (thisYear - 1));
  const result = text(elements.get('battery-return-result'));
  assert.match(result, /^123 kWh of grid electricity went back/);
  assert.match(result, /8\D?700 of 8\D?760 hours\. Counted with a 10\D2 kWh battery and at least 89 % of its charge coming back out\.$/);
  const rows = elements.get('battery-return-rows').children;
  assert.equal(rows.length, 12);
  assert.equal(rows[0].children[1].textContent, '1 kWh');
  assert.equal(elements.get('battery-return-meta').textContent, '123 kWh in ' + (thisYear - 1));
});

test('a year without a figure says why instead of showing zero', async () => {
  for (const [problem, said] of [
    ['battery_size', /^Set the battery's size in Settings/],
    ['batteries', /^FTW works this figure out for one battery with its size set\. In 2025 it read 2 batteries\./],
  ]) {
    const { elements, listeners, text } = harness({ year: 2025, returned_wh: 0, hours: 8760, complete_hours: 8000, problem, batteries: 2, months: [] });
    elements.get('battery-return-details').open = true;
    listeners.get('battery-return-details:toggle')();
    await tick();
    await tick();
    assert.match(text(elements.get('battery-return-result')), said);
    assert.equal(elements.get('battery-return-meta').textContent, 'No figure');
  }
});

test('a year without complete readings says so instead of showing zero', async () => {
  const { elements, listeners, text } = harness({ year: 2023, returned_wh: 0, hours: 8760, complete_hours: 0, months: [] });
  elements.get('battery-return-details').open = true;
  listeners.get('battery-return-details:toggle')();
  await tick();
  await tick();
  assert.equal(text(elements.get('battery-return-result')), 'FTW has no complete readings for 2023.');
  assert.equal(elements.get('battery-return-meta').textContent, 'No readings');
});
