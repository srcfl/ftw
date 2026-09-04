import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const webRoot = dirname(fileURLToPath(import.meta.url));
const weather = readFileSync(join(webRoot, 'settings', 'tabs', 'weather.js'), 'utf8');
const vendor = join(webRoot, 'vendor', 'terra-draw');

test('PV-array drawing loads Terra Draw from the vendored copy, not a CDN', () => {
  assert.doesNotMatch(weather, /unpkg\.com/);
  assert.match(weather, /\/vendor\/terra-draw\/terra-draw\.umd\.js/);
  assert.match(weather, /\/vendor\/terra-draw\/terra-draw-maplibre-gl-adapter\.umd\.js/);
});

test('vendored Terra Draw files are present', () => {
  for (const rel of [
    'terra-draw.umd.js',
    'terra-draw-maplibre-gl-adapter.umd.js',
    'LICENSE',
    'README.md',
  ]) {
    assert.ok(existsSync(join(vendor, rel)), rel + ' must be vendored');
  }
});
