import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const webRoot = dirname(fileURLToPath(import.meta.url));
const weather = readFileSync(join(webRoot, 'settings', 'tabs', 'weather.js'), 'utf8');
const vendor = join(webRoot, 'vendor', 'maplibre');

test('weather map loads MapLibre from the vendored copy, not a CDN', () => {
  assert.doesNotMatch(weather, /unpkg\.com/);
  assert.doesNotMatch(weather, /cdn\.jsdelivr\.net/);
  assert.match(weather, /\/vendor\/maplibre\//);
  assert.match(weather, /maplibre-gl\.mjs/);
  assert.match(weather, /maplibre-gl\.css/);
});

test('weather map sends a Referer on OSM tiles so volunteer servers do not 403', () => {
  assert.match(weather, /tile\.openstreetmap\.org/);
  assert.match(weather, /referrerPolicy:\s*"strict-origin-when-cross-origin"/);
  // The vendored build must actually honor a per-request referrerPolicy from
  // transformRequest, or the opt-in above is a no-op.
  const shared = readFileSync(join(vendor, 'maplibre-gl-shared.mjs'), 'utf8');
  assert.match(shared, /referrerPolicy/);
});

test('vendored MapLibre GL JS 6.7.0 files are present', () => {
  for (const rel of [
    'maplibre-gl.mjs',
    'maplibre-gl-shared.mjs',
    'maplibre-gl-worker.mjs',
    'maplibre-gl.css',
    'LICENSE.txt',
    'README.md',
  ]) {
    assert.ok(existsSync(join(vendor, rel)), rel + ' must be vendored');
  }
  // v6 is code-split: the entry imports its chunks by relative URL, so the
  // names above are load-bearing, not just files that happen to exist.
  const entry = readFileSync(join(vendor, 'maplibre-gl.mjs'), 'utf8');
  assert.match(entry, /maplibre-gl-shared\.mjs/);
  assert.match(entry, /maplibre-gl-worker\.mjs/);
});
