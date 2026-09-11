import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const webRoot = dirname(fileURLToPath(import.meta.url));
const weather = readFileSync(join(webRoot, 'settings', 'tabs', 'weather.js'), 'utf8');
const vendor = join(webRoot, 'vendor', 'leaflet');

test('weather map loads Leaflet from the vendored copy, not unpkg', () => {
  assert.doesNotMatch(weather, /unpkg\.com/);
  assert.match(weather, /\/vendor\/leaflet\/leaflet\.js/);
  assert.match(weather, /\/vendor\/leaflet\/leaflet\.css/);
});

test('weather map sends a Referer on OSM tiles so volunteer servers do not 403', () => {
  assert.match(weather, /tile\.openstreetmap\.org/);
  assert.match(weather, /referrerPolicy:\s*"strict-origin-when-cross-origin"/);
  const leaflet = readFileSync(join(vendor, 'leaflet.js'), 'utf8');
  assert.match(leaflet, /typeof this\.options\.referrerPolicy/);
});

test('vendored Leaflet 1.9.4 files are present', () => {
  for (const rel of [
    'leaflet.js',
    'leaflet.css',
    'LICENSE',
    'README.md',
    'images/marker-icon.png',
    'images/marker-icon-2x.png',
    'images/marker-shadow.png',
  ]) {
    assert.ok(existsSync(join(vendor, rel)), rel + ' must be vendored');
  }
});
