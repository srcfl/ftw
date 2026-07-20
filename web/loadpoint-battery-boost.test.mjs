import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./loadpoints.js', import.meta.url), 'utf8');

test('loadpoint UI exposes a bounded battery boost lease with reserve', () => {
  assert.match(source, /\/battery_boost/);
  assert.match(source, /min_battery_soc_pct: reserve/);
  assert.match(source, /duration_s: duration/);
  assert.match(source, /value="14400">4 h/);
  assert.match(source, /EV target %/);
  assert.match(source, /Departure/);
});

test('active boost and every core stop class are visible', () => {
  assert.match(source, /Battery boost.*ACTIVE/s);
  assert.match(source, /home reserve/);
  for (const reason of [
    'vehicle_unplugged', 'battery_reserve_reached', 'site_safety_block',
    'loadpoint_driver_unavailable', 'battery_hold', 'fuse_safety_block',
  ]) {
    assert.match(source, new RegExp(reason));
  }
});

test('operator clamps disable the start affordance', () => {
  assert.match(source, /!lp\.plugged_in \|\| lp\.manual_active \|\| lp\.surplus_only/);
  assert.match(source, /Turn off surplus-only first/);
  assert.match(source, /Release the loadpoint hold first/);
});
