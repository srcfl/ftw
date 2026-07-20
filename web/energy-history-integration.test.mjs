import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./energy-history.js', import.meta.url), 'utf8');

test('History contains one complete energy ledger surface', () => {
  const history = html.match(/<main id="view-history"[\s\S]*?<main id="view-more"/)?.[0] || '';
  for (const id of [
    'energy-history-asset', 'energy-history-range', 'energy-history-refresh',
    'energy-history-csv', 'energy-history-chart', 'energy-history-summary',
    'energy-history-rows',
  ]) {
    assert.equal((history.match(new RegExp(`id="${id}"`, 'g')) || []).length, 1, id);
  }
  assert.match(history, /<th>Quality<\/th>/);
  assert.match(history, /Plan decisions/);
  assert.equal((html.match(/energy-history\.js/g) || []).length, 1);
});

test('ledger chart uses returned buckets and ignores gaps and counter resets', () => {
  assert.match(source, /point\.bucket_start_ms/);
  assert.match(source, /point\.quality !== 'gap'/);
  assert.match(source, /point\.quality !== 'reset'/);
  assert.match(source, /window\.ftwEnergyHistoryLoad = load/);
});
