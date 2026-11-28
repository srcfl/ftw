// node --test web/driver-control.test.mjs
//
// Whether FTW can control a device, said where people choose and look at it.
// A new owner added a Zaptec Go through a read-only driver; FTW accepted the
// charger, never controlled it and never said why.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

globalThis.window = {};
await import('./driver-control.js');
await import('./settings/tabs/loadpoints.js');
const words = window.FTWDriverControl;
const S = window.FTWSettings;

const read = path => readFileSync(new URL(path, import.meta.url), 'utf8');
const escHtml = s => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');

// Reasons as Core serves them for drivers at srcfl/device-drivers c4c39af:
// the lead-in that repeats the note is gone, and sonnen and zap give none.
const WALL_REASON = 'Tesla offers no way for another system to set its current. Control goes through the car (tesla_vehicle) or a charger FTW can control, such as one on OCPP.';
const TESLA_CLOUD_REASON = "Tesla accepts a command only when it is signed with a key paired to the car, and this driver cannot sign. Use tesla_vehicle (BLE) to start the car's charge, or a charger FTW can control.";

describe('the words for a read-only driver', () => {
  it('names a charger, a car and a heat pump, and gives the driver its own reason', () => {
    const zaptec = words.readOnlyNote({ read_only: true, capabilities: ['ev'] });
    assert.equal(words.sentence(zaptec), 'FTW reads this charger but cannot control it.');
    const wall = words.readOnlyNote({ read_only: true, capabilities: ['ev'], read_only_reason: WALL_REASON });
    assert.equal(words.sentence(wall), 'FTW reads this charger but cannot control it. ' + WALL_REASON);
    const car = words.readOnlyNote({ read_only: true, capabilities: ['vehicle'], read_only_reason: TESLA_CLOUD_REASON });
    assert.equal(words.sentence(car), 'FTW reads this car but cannot control it. ' + TESLA_CLOUD_REASON);
    assert.match(words.sentence(words.readOnlyNote({ read_only: true, capabilities: ['heatpump'] })), /reads this heat pump/);
  });

  it('names a battery only when the battery is the driver\'s main job', () => {
    // sonnen at c4c39af: battery only, and no reason left after "Read-only.".
    assert.equal(words.sentence(words.readOnlyNote({ read_only: true, capabilities: ['battery'], read_only_reason: '' })),
      'FTW reads this battery but cannot control it.');
    assert.match(words.sentence(words.readOnlyNote({ read_only: true, capabilities: ['battery', 'meter'] })), /reads this battery/);
    // Zap, SMA and Fronius read a battery beside solar and the meter. FTW
    // reading them is no surprise, so they carry no note.
    assert.equal(words.readOnlyNote({ read_only: true, capabilities: ['meter', 'pv', 'battery'] }), null);
    assert.equal(words.readOnlyNote({ read_only: true, capabilities: ['battery', 'meter', 'pv'] }), null);
  });

  it('says nothing for a driver that controls, or a meter or solar inverter that is only read', () => {
    assert.equal(words.readOnlyNote({ read_only: false, capabilities: ['ev'] }), null);
    assert.equal(words.readOnlyNote({ capabilities: ['ev'] }), null);
    assert.equal(words.readOnlyNote({ read_only: true, capabilities: ['meter'] }), null);
    assert.equal(words.readOnlyNote({ read_only: true, capabilities: ['meter', 'pv'] }), null);
    assert.equal(words.readOnlyNote(null), null);
    assert.equal(words.sentence(null), '');
  });

  it('reads a loadpoint by its own field names', () => {
    assert.equal(words.loadpointNote({ driver_read_only: false }), null);
    assert.equal(words.loadpointNote(null), null);
    assert.equal(words.sentence(words.loadpointNote({ driver_read_only: true, driver_read_only_reason: WALL_REASON })),
      'FTW reads this charger but cannot control it. ' + WALL_REASON);
    assert.equal(words.sentence(words.loadpointNote({ driver_read_only: true })),
      'FTW reads this charger but cannot control it.');
  });
});

describe('Settings → Chargers', () => {
  const render = config => S.tabs.loadpoints.render({ config, escHtml, help: () => '' });
  const config = () => ({
    drivers: [
      { name: 'zaptec', lua: 'drivers/zaptec_cloud.lua' },
      { name: 'easee', lua: 'drivers/easee_cloud.lua' },
    ],
    loadpoints: [{ id: 'drive', driver_name: 'zaptec' }, { id: 'street', driver_name: 'easee' }],
  });

  it('says on the charger card, and in the charger list, that FTW cannot control a read-only charger', () => {
    S.ocppStatus = { enabled: false, chargers: [] };
    S.catalogByLua = {
      'drivers/zaptec_cloud.lua': { capabilities: ['ev'], read_only: true },
      'drivers/easee_cloud.lua': { capabilities: ['ev'] },
    };
    const html = render(config());
    assert.match(html, /data-charger-read-only="0" role="note">FTW reads this charger but cannot control it\.</);
    assert.doesNotMatch(html, /data-charger-read-only="1"/);
    assert.match(html, /<option value="zaptec" selected>zaptec \(reads only\)<\/option>/);
    assert.match(html, /<option value="easee">easee<\/option>/);
  });

  it('escapes the driver reason', () => {
    S.catalogByLua = {
      'drivers/zaptec_cloud.lua': { capabilities: ['ev'], read_only: true, read_only_reason: '<b>held back</b>' },
      'drivers/easee_cloud.lua': { capabilities: ['ev'] },
    };
    const html = render(config());
    assert.match(html, /cannot control it\. &lt;b&gt;held back&lt;\/b&gt;/);
    assert.doesNotMatch(html, /<b>held back/);
  });
});

describe('the other places the owner looks', () => {
  it('loads the shared words before every view that uses them', () => {
    const index = read('./index.html');
    const at = name => index.indexOf(name);
    assert.ok(at('/driver-control.js') > 0 && at('/driver-control.js') < at('/app.js'));
    assert.ok(at('/driver-control.js') < at('/settings/tabs/devices.js'));
    assert.ok(at('/driver-control.js') < at('/device-flow.js') && at('/device-flow.js') < at('/settings/tabs/devices.js'));
    const setup = read('./setup.html');
    assert.ok(setup.indexOf('/driver-control.js') > 0 && setup.indexOf('/driver-control.js') < setup.indexOf('/device-flow.js'));
    assert.ok(setup.indexOf('/device-flow.js') < setup.indexOf('/setup.js'));
  });

  it('shows the note on the device row, the catalog card, setup and the EV card', () => {
    const devices = read('./settings/tabs/devices.js');
    assert.match(devices, /class="drv-module-readonly-note read-only-note" role="note"/);
    assert.match(devices, /opts\.readOnlyNoteEl\.textContent = readOnlySentence\(note\)/,
      'a version switch must update the note with the badge');
    // The catalog card is shared by Settings and setup (device-flow.js).
    const flow = read('./device-flow.js');
    assert.match(flow, /if \(readsOnly\(e\)\) tag\('reads only', 'drv-catalog-unproven', readOnlySentence\(e\)\)/);
    assert.match(devices, /flow\(\)\.card\(e, \{/);
    const setup = read('./setup.js');
    assert.match(setup, /flow\(\)\.card\(entry, \{/);
    assert.match(setup, /readOnlyEl\.textContent = readOnly;/, 'the device form says it before the device is added');
    const app = read('./app.js');
    assert.match(app, /window\.FTWDriverControl\.loadpointNote\(matched\)/);
    assert.match(app, /readOnlyNote\.className = "ev-read-only-note read-only-note"/);
    // No Charge now or Pause charging for a charger FTW only reads, unless a
    // hold from before still needs returning to the plan.
    assert.match(app, /var readOnlyIdle = nextLp\.driver_read_only === true && !nextLp\.manual_active &&\s+!nextLp\.manual_restore_unconfirmed;/);
    assert.match(app, /manual\.el\.hidden = !nextLp\.plugged_in \|\| readOnlyIdle;/);
    assert.match(read('./loadpoints.js'), /lp\.driver_read_only \? ' · reads only, FTW cannot control it'/);
  });
});

describe('the EV card for a charger FTW only reads', () => {
  const app = read('./app.js');
  const evControls = app.slice(app.indexOf('function buildEvControls'), app.indexOf('// The ready time as "HH:MM"'));
  const fake = () => ({
    style: {}, dataset: {}, children: [], hidden: false, textContent: '',
    appendChild(child) { this.children.push(child); return child; },
    setAttribute() {},
  });
  const build = new Function('document', 'buildManualChargeSection', 'buildScheduleSection', 'buildPVModeSection',
    'readyHHMM', 'evIsPaused', evControls + '; return buildEvControls;');
  const manual = { el: fake(), update() {} };
  const buildEvControls = build(
    { createElement: fake },
    () => manual,
    () => Object.assign(fake(), { update() {} }),
    () => null,
    () => '06:00',
    () => false,
  );
  const lp = extra => Object.assign({ id: 'drive', plugged_in: true, schedule: {} }, extra);

  it('offers no Charge now or Pause charging', () => {
    const controls = buildEvControls(lp({ driver_read_only: true }), false, true);
    assert.equal(manual.el.hidden, true);
    controls.update(lp({ driver_read_only: false }), null);
    assert.equal(manual.el.hidden, false, 'a charger FTW controls keeps its buttons');
  });

  it('still lets a hold from before return to the plan', () => {
    const controls = buildEvControls(lp({ driver_read_only: true, manual_active: true }), false, true);
    assert.equal(manual.el.hidden, false);
    controls.update(lp({ driver_read_only: true, manual_restore_unconfirmed: true }), null);
    assert.equal(manual.el.hidden, false);
    controls.update(lp({ driver_read_only: true, plugged_in: false }), null);
    assert.equal(manual.el.hidden, true);
  });
});
