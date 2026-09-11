import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

// commanded_reason (#1009): the plan strip names the exact clamp
// behind a 0 W command instead of the generic "paused by the box".

test('every pause reason has its own sentence', () => {
  assert.match(source, /commanded_reason === "fuse_cooldown" \|\| lp\.commanded_reason === "fuse_limit"/);
  assert.match(source, /Paused: main-fuse protection/);
  assert.match(source, /commanded_reason === "site_meter_stale"/);
  assert.match(source, /Paused for safety: site-meter data is stale/);
  assert.match(source, /commanded_reason === "pv_surplus_pause"/);
  assert.match(source, /Paused: waiting for PV surplus/);
  // A fuse-limited ongoing charge says so too.
  assert.match(source, /Rate is limited by the main fuse right now/);
});
