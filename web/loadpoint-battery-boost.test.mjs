import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const app = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const devices = readFileSync(new URL('./loadpoints.js', import.meta.url), 'utf8');
const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const boostView = app.slice(
  app.indexOf('function buildEvBoostView'),
  app.indexOf('function buildManualChargeSection'),
);

// One way to let the home battery help the car (#1063): the time-boxed
// per-loadpoint lease, in the EV modal, with a reserve. The site-wide
// battery_covers_ev toggle is gone from the modal.

test('the EV modal owns the battery boost lease', () => {
  assert.match(boostView, /\/battery_boost/);
  assert.match(boostView, /min_battery_soc: r \/ 100/);
  assert.match(boostView, /duration_s: parseInt\(dur\.value, 10\)/);
  // No third SoC target and no departure field: the lease is reserve + time.
  assert.doesNotMatch(boostView, /ev_target_soc/);
  assert.doesNotMatch(boostView, /departure/);
  // Start is refused for the same reasons the controller refuses it.
  assert.match(boostView, /Plug in the car first/);
  assert.match(boostView, /Stop the manual charge first/);
  assert.match(boostView, /Turn off PV only first/);
  // Active state and stop.
  assert.match(boostView, /method: "DELETE"/);
  assert.match(boostView, /left · home battery kept above/);
});

test('every controller stop reason has words', () => {
  for (const reason of [
    'cancelled', 'expired', 'vehicle_unplugged', 'ev_target_reached', 'departure_reached',
    'operator_hold', 'surplus_only', 'site_safety_block', 'loadpoint_driver_unavailable',
    'battery_unavailable', 'battery_reserve_reached', 'battery_hold', 'core_mode',
    'fuse_safety_block', 'restart_lease_invalid',
  ]) {
    assert.match(app, new RegExp(reason + ': "'));
  }
});

test('the boost view is mounted last in the modal and updated on polls', () => {
  assert.match(app, /evBoostEl = buildEvBoostView\(matched, status\)/);
  assert.match(app, /evBoostEl\.update\(matched, status\)/);
  assert.match(app, /evModalBody\.lastChild !== evBoostDetails/);
});

test('the legacy site-wide cover left the modal but is never hidden while on', () => {
  assert.doesNotMatch(html, /battery-covers-ev-toggle/);
  assert.doesNotMatch(html, /Legacy site-wide battery cover/);
  assert.doesNotMatch(app, /bceToggle/);
  assert.match(boostView, /Site-wide battery cover is on \(older setting\)/);
  assert.match(boostView, /setBatteryCoversEV\(false\)/);
  assert.match(boostView, /statusNow\.battery_covers_ev/);
});

test('the Devices card only reports a boost', () => {
  assert.doesNotMatch(devices, /lp-boost-enable/);
  assert.doesNotMatch(devices, /lp-boost-cancel/);
  assert.doesNotMatch(devices, /boostRequest/);
  assert.match(devices, /Battery boost.*ACTIVE/s);
  assert.match(devices, /stop it from the EV modal/);
  assert.match(devices, /Last boost stop/);
});
