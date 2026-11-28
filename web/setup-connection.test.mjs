import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./setup.html', import.meta.url), 'utf8');
// The page loads the shared words and the shared device flow before the
// guide, so the harness does too.
const source = ['./driver-control.js', './device-flow.js', './setup.js']
  .map(file => readFileSync(new URL(file, import.meta.url), 'utf8')).join('\n');

// Run the wizard's real navigation and save handlers. Initial field values
// come from the HTML so a changed default also changes the posted config.
// replies maps a path to the JSON the box answers with.
function wizard(search = '', zonesFail = false, catalog = [], replies = {}) {
  const elements = {}, requests = [];
  function element(attrs = '', tag = '') {
    const attr = name => attrs.match(new RegExp('\\b' + name + '="([^"]*)"'))?.[1];
    const handlers = {}, classes = new Set();
    let markup = '', id = '', value = attr('value') ?? '';
    const el = {
      tag, checked: /\bchecked\b/.test(attrs), style: {}, handlers, dataset: {},
      hidden: /\bhidden\b/.test(attrs), open: false, textContent: '', children: [], disabled: false,
      get id() { return id; },
      set id(v) { id = v; elements[v] = el; },
      get value() {
        if (tag === 'select' && value === '' && el.children.length && !el.children.some(c => c.value === '')) return el.children[0].value;
        return value;
      },
      set value(v) { value = String(v); },
      get options() { return el.children; },
      get className() { return [...classes].join(' '); },
      set className(v) { classes.clear(); String(v).split(/\s+/).filter(Boolean).forEach(c => classes.add(c)); },
      classList: { add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c) },
      addEventListener(event, fn) { (handlers[event] ??= []).push(fn); },
      fire(event) { for (const fn of handlers[event] ?? []) fn.call(el); },
      click() { el.fire('click'); },
      focus() { el.focused = true; },
      setAttribute(name, v) { el[name] = v; },
      set innerHTML(v) { markup = v; el.children = []; if (tag === 'select') value = ''; },
      get innerHTML() { return markup || el.textContent.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'); },
      appendChild(child) {
        el.children.push(child);
        if (tag === 'select' && (child.selected || el.children.length === 1)) value = child.value;
        return child;
      },
      checkValidity() {
        if (!value) return !/\brequired\b/.test(attrs);
        const n = Number(value), step = attr('step');
        return Number.isFinite(n) && (attr('min') == null || n >= Number(attr('min'))) &&
          (attr('max') == null || n <= Number(attr('max'))) && (step !== '1' || Number.isInteger(n));
      },
      // Every text node below this one, as a reader sees it.
      text() { return [el.textContent, ...el.children.map(c => c.text())].join(' '); },
    };
    return el;
  }
  for (const match of html.matchAll(/<([a-z][a-z0-9-]*)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    element(match[2], match[1]).id = match[3];
  }
  for (const match of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
    for (const o of match[2].matchAll(/<option\b([^>]*)>([^<]*)/g)) {
      const opt = element(o[1], 'option');
      opt.value = o[1].match(/value="([^"]*)"/)?.[1] ?? '';
      opt.textContent = o[2];
      opt.selected = /\bselected\b/.test(o[1]);
      elements[match[1]].appendChild(opt);
    }
  }
  const reply = body => Promise.resolve({ ok: true, status: 200, json: async () => body });
  const sandbox = {
    location: { search }, scrollTo() {}, URLSearchParams, setTimeout() {}, console,
    document: {
      getElementById: id => elements[id] ?? null, createElement: tag => element('', tag),
      querySelectorAll: selector => selector === '.step' ? Object.entries(elements).filter(([id]) => /^step-\d+$/.test(id)).map(([, el]) => el) : [],
    },
    fetch(path, opts) {
      requests.push({ path, opts });
      if (zonesFail && path === '/api/prices/zones') return Promise.reject(new Error('offline'));
      if (path === '/api/drivers/catalog') return reply({ entries: catalog });
      if (path in replies) return reply(typeof replies[path] === 'function' ? replies[path](opts) : replies[path]);
      return reply(path === '/api/prices/zones' ? { zones: [
        { country: 'Sweden', code: 'SE3', currency: 'SEK', name: 'SE3 — Stockholm' },
        { country: 'Sweden', code: 'SE4', currency: 'SEK', name: 'SE4 — Malmö' },
        { country: 'Belgium', code: 'BE', currency: 'EUR' },
      ] } : {});
    },
  };
  sandbox.window = sandbox;
  vm.runInNewContext(source, sandbox);
  const posts = path => requests.filter(r => r.opts?.method === 'POST' && r.path === path);
  return { elements, requests, go: sandbox.goStep, save: sandbox.saveConfig, sandbox,
    choose(id, value, event = 'change') { elements[id].value = value; elements[id].fire(event); },
    posted: () => JSON.parse(posts('/api/config')[0].opts.body),
    saved: () => posts('/api/config').length > 0,
    tests: () => posts('/api/drivers/test').map(r => JSON.parse(r.opts.body)),
    visible: n => elements['step-' + n].classList.contains('visible'),
    // The catalog card for a driver id, as the owner sees it.
    card: id => elements['catalog-results'].children.find(c => c.children[0].textContent === catalog.find(e => e.id === id).name),
  };
}
const settled = () => new Promise(resolve => setImmediate(resolve));

describe('setup electricity connection', () => {
  it('requires the main fuse instead of silently saving 16 A', () => {
    const rig = wizard('?step=2');
    assert.equal(rig.elements['fuse-amps'].value, '');
    rig.go(3);
    assert.equal(rig.visible(2), true);
    assert.equal(rig.elements['connection-error'].hidden, false);
    rig.save();
    assert.equal(rig.saved(), false);
  });

  for (const choice of ['3', '1']) {
    it('saves ' + choice + ' phases at 230 V and the entered fuse rating', () => {
      const rig = wizard('?step=2');
      rig.choose('site-connection', choice);
      rig.elements['fuse-amps'].value = '20';
      rig.go(3);
      assert.equal(rig.visible(3), true);
      rig.save();
      assert.deepEqual(rig.posted().fuse, { phases: Number(choice), voltage: 230, max_amps: 20 });
    });
  }

  it('shows help for an unknown connection and blocks navigation/save', () => {
    const rig = wizard('?step=2');
    rig.elements['fuse-amps'].value = '20';
    rig.choose('site-connection', 'unknown');
    assert.match(rig.elements['connection-help'].textContent, /network operator/);
    rig.go(3);
    rig.save();
    assert.equal(rig.visible(2), true);
    assert.equal(rig.saved(), false);
    rig.choose('site-connection', '1');
    rig.go(3);
    assert.equal(rig.visible(3), true);
  });

  for (const amps of ['', '0', '-1', '101', '1.5', 'NaN']) {
    it('rejects invalid fuse rating ' + JSON.stringify(amps) + ' even from a review deep link', () => {
      const rig = wizard('?step=8');
      rig.elements['fuse-amps'].value = amps;
      rig.save();
      assert.equal(rig.visible(2), true);
      assert.equal(rig.saved(), false);
    });
  }

  it('keeps custom values across country changes, back/forward and save', async () => {
    const rig = wizard('?step=2');
    await settled();
    rig.elements['fuse-amps'].value = '25';
    rig.choose('site-connection', 'custom');
    rig.choose('fuse-phases', '2');
    rig.choose('fuse-voltage', '240', 'input');
    rig.choose('price-country', 'Belgium');
    assert.equal(rig.elements['site-connection'].value, 'custom');
    assert.match(rig.elements['connection-help'].textContent, /240 V/);
    rig.go(3); rig.go(2); rig.go(5); rig.save();
    assert.deepEqual(rig.posted().fuse, { phases: 2, voltage: 240, max_amps: 25 });
    assert.equal(rig.posted().price.currency, 'EUR');
    assert.match(rig.elements['review-content'].innerHTML, /Custom connection.*240 V/);
  });

  it('only restores 230 V when a standard connection is explicitly selected', () => {
    const rig = wizard('?step=2');
    rig.choose('fuse-voltage', '400', 'input');
    assert.equal(rig.elements['site-connection'].value, 'custom');
    rig.choose('price-country', 'Norway');
    assert.equal(rig.elements['fuse-voltage'].value, '400');
    rig.choose('site-connection', '3');
    assert.equal(rig.elements['fuse-voltage'].value, '230');
    assert.match(rig.elements['connection-help'].textContent, /without neutral/);
  });

  it('rejects an empty expert voltage and opens the field with an explanation', () => {
    const rig = wizard('?step=8');
    rig.elements['fuse-amps'].value = '20';
    rig.choose('fuse-voltage', '', 'input');
    rig.save();
    assert.equal(rig.visible(2), true);
    assert.equal(rig.elements['connection-advanced'].open, true);
    assert.equal(rig.saved(), false);
  });

  it('keeps the Swedish fallback usable if the country lookup fails', async () => {
    const rig = wizard('?step=2', true);
    await settled();
    rig.elements['fuse-amps'].value = '16';
    rig.choose('price-zone', 'SE4');
    rig.save();
    assert.equal(rig.posted().price.zone, 'SE4');
    assert.deepEqual(rig.posted().fuse, { phases: 3, voltage: 230, max_amps: 16 });
  });
});

describe('setup price area', () => {
  it('chooses no area for the owner and saves no SE3 they never picked', async () => {
    const rig = wizard('?step=2');
    await settled();
    assert.equal(rig.elements['price-zone'].value, '');
    assert.equal(rig.elements['price-zone-group'].hidden, false, 'Sweden has four areas, so it asks');
    rig.elements['fuse-amps'].value = '20';
    rig.go(5);
    assert.match(rig.elements['review-content'].innerHTML, /no price area/);
    assert.match(rig.elements['review-content'].innerHTML, /No price area\. FTW will not fetch prices/);
    rig.save();
    assert.equal(rig.posted().price, undefined, 'a provider without an area would run on SE3');
  });

  it('asks nothing in a country with one area', async () => {
    const rig = wizard('?step=2');
    await settled();
    rig.choose('price-country', 'Belgium');
    assert.equal(rig.elements['price-zone'].value, 'BE');
    assert.equal(rig.elements['price-zone-group'].hidden, true);
    rig.elements['fuse-amps'].value = '20';
    rig.go(5);
    assert.doesNotMatch(rig.elements['review-content'].innerHTML, /No price area/);
    rig.save();
    assert.deepEqual(rig.posted().price, { provider: 'sourceful', zone: 'BE', currency: 'EUR' });
  });

  it('names the chosen area on the review', async () => {
    const rig = wizard('?step=2');
    await settled();
    rig.choose('price-zone', 'SE4');
    rig.elements['fuse-amps'].value = '20';
    rig.go(5);
    assert.match(rig.elements['review-content'].innerHTML, /price area SE4 · Malmö/);
  });
});

describe('setup planner on a new box', () => {
  it('starts planning and does not sell from the battery', () => {
    const rig = wizard('?step=2');
    rig.elements['fuse-amps'].value = '20';
    rig.save();
    assert.deepEqual(rig.posted().planner, { enabled: true, battery_export: 'not_allowed' });
  });
});

// Settings' device flow in the guide: find the driver, give the address,
// and test the device when it is added.
const SUNGROW = { id: 'sungrow', name: 'Sungrow SH Hybrid Inverter', manufacturer: 'Sungrow', filename: 'sungrow.lua',
  path: 'drivers/sungrow.lua', protocols: ['modbus'], capabilities: ['meter', 'pv', 'battery'],
  connection_defaults: { port: 502, unit_id: 1 }, tested_models: ['SH10RT'],
  setup_hint: "Turn on Modbus TCP in the inverter's settings." };
const HUAWEI = { id: 'huawei', name: 'Huawei SUN2000', manufacturer: 'Huawei', filename: 'huawei.lua',
  path: 'drivers/huawei.lua', protocols: ['modbus'], capabilities: ['meter', 'pv', 'battery'], read_only: true,
  connection_defaults: { port: 502, unit_id: 1 } };
const SDM630 = { id: 'sdm630', name: 'Eastron SDM630', manufacturer: 'Eastron', filename: 'sdm630.lua',
  path: 'drivers/sdm630.lua', protocols: ['modbus'], capabilities: ['meter'], read_only: true };
const ZAP = { id: 'zap', name: 'Sourceful Zap', manufacturer: 'Sourceful', filename: 'zap.lua', path: 'drivers/zap.lua',
  protocols: ['http'], capabilities: ['meter', 'pv', 'battery'], read_only: true,
  connection_defaults: { host: 'zap.local', port: 80 } };
const EASEE = { id: 'easee_cloud', name: 'Easee Cloud', manufacturer: 'Easee', filename: 'easee_cloud.lua',
  path: 'drivers/easee_cloud.lua', protocols: ['http'], capabilities: ['ev'], http_hosts: ['api.easee.com'],
  config_secrets: ['password'] };
const CATALOG = [SUNGROW, HUAWEI, SDM630, ZAP, EASEE];
const flush = async () => { for (let i = 0; i < 6; i++) await settled(); };
const tags = card => card.children.find(c => c.className === 'drv-catalog-tags').children.map(t => t.textContent);

async function devicesStep(replies = {}) {
  const rig = wizard('?step=2', false, CATALOG, replies);
  await settled();
  rig.elements['fuse-amps'].value = '20';
  rig.choose('price-zone', 'SE3');
  rig.go(3);
  await flush();
  return rig;
}

describe('setup devices — one flow with Settings', () => {
  it('lists the catalog as cards, with the setup hint and reads only', async () => {
    const rig = await devicesStep();
    assert.equal(rig.visible(3), true);
    assert.match(rig.card('sungrow').text(), /Turn on Modbus TCP in the inverter's settings\./);
    assert.ok(!tags(rig.card('sungrow')).includes('reads only'), 'unstated read_only is unknown: say nothing');
    assert.ok(tags(rig.card('huawei')).includes('reads only'), 'a read-only hybrid says so');
    assert.ok(!tags(rig.card('sdm630')).includes('reads only'), 'a meter is only ever read');
    rig.choose('catalog-search', 'SH10RT', 'input');
    assert.deepEqual(rig.elements['catalog-results'].children.map(c => c.children[0].textContent), ['Sungrow SH Hybrid Inverter']);
  });

  it('asks only the address; the driver knows the port and Unit ID', async () => {
    const rig = await devicesStep();
    rig.card('sungrow').click();
    assert.equal(rig.elements['device-form'].hidden, false);
    assert.equal(rig.elements['device-hint'].textContent, "Turn on Modbus TCP in the inverter's settings.");
    assert.equal(rig.elements['dev-port'].value, '502');
    assert.equal(rig.elements['dev-unit'].value, '1');
    assert.equal(rig.elements['dev-site-meter'].checked, true, 'the first meter measures the home');
    rig.sandbox.addDevice();
    assert.match(rig.elements['device-test'].textContent, /Enter the device address/);
    assert.equal(rig.tests().length, 0);
  });

  it('adds a device only after its test reads it', async () => {
    const rig = await devicesStep({ '/api/drivers/test': { ok: true, readings: [
      { type: 'meter', smoothed_w: 1200 }, { type: 'pv', smoothed_w: 3400 }, { type: 'battery', smoothed_w: -500, soc: 0.52 }] } });
    rig.card('sungrow').click();
    rig.elements['dev-host'].value = '192.168.1.20';
    rig.elements['dev-battery'].value = '9.6';
    rig.sandbox.addDevice();
    assert.match(rig.elements['device-test'].innerHTML, /Testing the connection/);
    await flush();
    const tested = rig.tests()[0];
    assert.deepEqual(tested.capabilities, { modbus: { host: '192.168.1.20', port: 502, unit_id: 1 } });
    assert.equal(tested.lua, 'drivers/sungrow.lua');
    assert.equal(rig.elements['device-form'].hidden, true);
    assert.match(rig.elements['device-list'].text(), /Connected · grid 1\.2 kW in · solar 3\.4 kW · battery 52 %/);
    rig.go(4); rig.go(5);
    assert.match(rig.elements['review-content'].innerHTML, /Connected/);
    rig.save();
    const [driver] = rig.posted().drivers;
    assert.deepEqual(driver, { name: 'sungrow', lua: 'drivers/sungrow.lua', is_site_meter: true, battery_capacity_wh: 9600,
      capabilities: { modbus: { host: '192.168.1.20', port: 502, unit_id: 1 } } });
  });

  it('keeps a device that does not answer out, with the hint, until Add anyway', async () => {
    const rig = await devicesStep({ '/api/drivers/test': { ok: false, error: 'dial tcp 192.168.1.20:503: connect: connection refused' } });
    rig.card('sungrow').click();
    rig.elements['dev-host'].value = '192.168.1.20';
    rig.elements['dev-port'].value = '503';
    rig.sandbox.addDevice();
    await flush();
    assert.equal(rig.tests()[0].capabilities.modbus.port, 503);
    const box = rig.elements['device-test'];
    assert.equal(box.hidden, false);
    assert.match(box.text(), /FTW could not read this device/);
    assert.match(box.text(), /Turn on Modbus TCP in the inverter's settings\./);
    assert.match(box.text(), /connection refused/);
    assert.equal(rig.elements['device-add'].textContent, 'Test again');
    assert.equal(rig.elements['device-add-anyway'].hidden, false);
    assert.equal(rig.elements['device-list'].children.length, 0, 'a failed device is not saved');
    rig.sandbox.addDeviceAnyway();
    assert.match(rig.elements['device-list'].text(), /Not tested: it did not answer/);
    rig.save();
    assert.equal(rig.posted().drivers[0].capabilities.modbus.port, 503);
  });

  it('uses what the scan found: the driver, the address and its port', async () => {
    const rig = await devicesStep({ '/api/scan?fingerprint=1': [
      { ip: '192.168.1.30', port: 1502, protocol: 'modbus', hostname: 'sungrow.local', matches: [{ driver: 'sungrow.lua', name: 'Sungrow SH10RT' }] }] });
    rig.sandbox.startScan();
    await flush();
    const row = rig.elements['scan-results'].children[0];
    assert.match(row.text(), /Sungrow SH10RT · sungrow\.local:1502/);
    row.children[1].click();
    assert.equal(rig.elements['device-title'].textContent, 'Sungrow SH Hybrid Inverter');
    assert.equal(rig.elements['dev-host'].value, 'sungrow.local', 'a .local name follows the device');
    assert.equal(rig.elements['dev-port'].value, '1502');
    assert.match(rig.elements['dev-host-hint'].textContent, /finds this device by name/);
  });

  it('says the fixed-address tip in one calm line', async () => {
    const rig = await devicesStep();
    rig.card('sungrow').click();
    rig.choose('dev-host', '192.168.1.20', 'input');
    assert.equal(rig.elements['dev-host-hint'].textContent, 'Tip: give this device a fixed address in your router.');
  });

  it('saves Zap PV and battery reads as off, not missing', async () => {
    const rig = await devicesStep({ '/api/drivers/test': { ok: true, readings: [{ type: 'meter', smoothed_w: 300 }] } });
    rig.card('zap').click();
    assert.equal(rig.elements['dev-host'].value, 'zap.local');
    assert.ok(!rig.elements['dev-battery'], 'a read-only gateway offers no battery control');
    rig.sandbox.addDevice();
    await flush();
    rig.save();
    const zap = rig.posted().drivers[0];
    assert.deepEqual(zap.config, { host: 'zap.local', read_pv: false, read_battery: false });
    assert.equal(zap.battery_telemetry_only, true);
  });

  it('adds a cloud charger as a device: sign in, pick it, test it', async () => {
    const rig = await devicesStep({
      '/api/ev/chargers': [{ id: 'EH123456', name: 'Garage' }],
      '/api/drivers/test': { ok: true, readings: [{ type: 'ev', smoothed_w: 0 }] },
    });
    rig.card('easee_cloud').click();
    assert.ok(!rig.elements['dev-site-meter'], 'a charger is never the site meter');
    rig.elements['dev-email'].value = 'owner@example.com';
    rig.elements['dev-password'].value = 'pw';
    rig.elements['dev-find-chargers'].click();
    await flush();
    const ask = rig.requests.find(r => r.path === '/api/ev/chargers');
    assert.deepEqual(JSON.parse(ask.opts.body), { provider: 'easee', email: 'owner@example.com', password: 'pw' });
    assert.equal(rig.elements['dev-serial-group'].hidden, false);
    rig.sandbox.addDevice();
    await flush();
    assert.deepEqual(rig.tests()[0].config, { email: 'owner@example.com', password: 'pw', serial: 'EH123456' });
    assert.match(rig.elements['device-list'].text(), /Connected · charger 0 W/);
  });

  it('keeps one site meter and unique names', async () => {
    const rig = await devicesStep({ '/api/drivers/test': { ok: true, readings: [{ type: 'meter', smoothed_w: 1 }] } });
    for (const host of ['192.168.1.20', '192.168.1.21']) {
      rig.card('sungrow').click();
      rig.elements['dev-host'].value = host;
      rig.elements['dev-site-meter'].checked = true;
      rig.sandbox.addDevice();
      await flush();
    }
    rig.save();
    const drivers = rig.posted().drivers;
    assert.deepEqual(drivers.map(d => d.name), ['sungrow', 'sungrow_2']);
    assert.deepEqual(drivers.map(d => d.is_site_meter), [false, true]);
  });

  it('builds a hostile secret key and name as text, never markup', async () => {
    const HOSTILE = 'token" autofocus onfocus="alert(1)';
    const rig = wizard('?step=2', false, [{ id: 'sonnen', name: '<img src=x onerror=alert(1)>', path: 'drivers/sonnen.lua',
      protocols: ['http'], capabilities: ['battery'], read_only: true, connection_defaults: { host: '' }, config_secrets: [HOSTILE] }]);
    rig.elements['fuse-amps'].value = '20';
    rig.go(3);
    await flush();
    rig.elements['catalog-results'].children[0].click();
    const input = rig.elements['dev-secret-' + HOSTILE];
    assert.ok(input, 'the input is found by the raw key, set as a property');
    assert.equal(input.type, 'password');
    for (const el of Object.values(rig.elements)) {
      // Text reads back escaped; only markup could hold a live tag or handler.
      assert.doesNotMatch(el.innerHTML, /<img|onfocus=/, 'no markup carries the key or the name');
    }
    assert.equal(rig.elements['device-title'].textContent, '<img src=x onerror=alert(1)>');
  });

  it('has no list of chargers of its own', () => {
    const js = readFileSync(new URL('./setup.js', import.meta.url), 'utf8');
    const page = readFileSync(new URL('./setup.html', import.meta.url), 'utf8');
    assert.doesNotMatch(js + page, /ev-provider|EV_PROVIDERS|ev_charger/);
  });
});

describe('setup start and end', () => {
  it('has no AI-assistant card on the welcome page', () => {
    assert.doesNotMatch(html, /assistant-setup|Start assistant setup|startAssistantSetup/);
  });

  it('ends with one line to the iPhone app and pairing', () => {
    assert.match(html, /href="https:\/\/apps\.apple\.com\/app\/ftw-energy\/id6822024949"/);
    assert.match(html, /pair it from Settings → FTW app/);
  });

  it('uses no expert words in the guide', () => {
    const js = readFileSync(new URL('./setup.js', import.meta.url), 'utf8');
    const words = (html + js).replace(/<!--[\s\S]*?-->/g, '').replace(/\/\/.*$/gm, '');
    assert.doesNotMatch(words, /\bMPC\b|\bSoC\b|set price\.zone/);
  });
});
