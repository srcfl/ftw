import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./setup.html', import.meta.url), 'utf8');
const source = readFileSync(new URL('./setup.js', import.meta.url), 'utf8');

// Run the wizard's real navigation and save handlers. Initial field values
// come from the HTML so a changed default also changes the posted config.
function wizard(search = '', zonesFail = false) {
  const elements = {}, requests = [];
  function element(attrs = '') {
    const attr = name => attrs.match(new RegExp('\\b' + name + '="([^"]*)"'))?.[1];
    const handlers = {}, classes = new Set();
    let markup = '';
    return {
      value: attr('value') ?? '', checked: /\bchecked\b/.test(attrs), style: {}, handlers,
      hidden: /\bhidden\b/.test(attrs), open: false, textContent: '', children: [],
      classList: { add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c) },
      addEventListener(event, fn) { (handlers[event] ??= []).push(fn); },
      fire(event) { for (const fn of handlers[event] ?? []) fn.call(this); },
      focus() { this.focused = true; },
      set innerHTML(value) { markup = value; this.children = []; },
      get innerHTML() { return markup || this.textContent.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'); },
      appendChild(child) { this.children.push(child); if (child.selected || this.children.length === 1) this.value = child.value; },
      checkValidity() {
        if (!this.value) return !/\brequired\b/.test(attrs);
        const n = Number(this.value), step = attr('step');
        return Number.isFinite(n) && (attr('min') == null || n >= Number(attr('min'))) &&
          (attr('max') == null || n <= Number(attr('max'))) && (step !== '1' || Number.isInteger(n));
      },
    };
  }
  for (const match of html.matchAll(/<([a-z-]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    elements[match[3]] = element(match[2]);
  }
  for (const match of html.matchAll(/<select\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
    const options = [...match[2].matchAll(/<option\b([^>]*)>/g)];
    const selected = options.find(o => /\bselected\b/.test(o[1])) ?? options[0];
    if (!selected) continue;
    elements[match[1]].value = selected[1].match(/value="([^"]*)"/)?.[1] ?? '';
  }
  const sandbox = {
    location: { search }, scrollTo() {}, URLSearchParams, setTimeout() {}, console,
    document: {
      getElementById: id => elements[id], createElement: () => element(),
      querySelectorAll: selector => selector === '.step' ? Object.entries(elements).filter(([id]) => /^step-\d+$/.test(id)).map(([, el]) => el) : [],
    },
    fetch(path, opts) {
      requests.push({ path, opts });
      if (zonesFail && path === '/api/prices/zones') return Promise.reject(new Error('offline'));
      return Promise.resolve({ ok: true, json: async () => path === '/api/prices/zones' ? { zones: [
        { country: 'Sweden', code: 'SE3', currency: 'SEK' },
        { country: 'Belgium', code: 'BE', currency: 'EUR' },
      ] } : {} });
    },
  };
  sandbox.window = sandbox;
  vm.runInNewContext(source, sandbox);
  return { elements, requests, go: sandbox.goStep, save: sandbox.saveConfig,
    choose(id, value, event = 'change') { elements[id].value = value; elements[id].fire(event); },
    posted: () => JSON.parse(requests.find(r => r.opts?.method === 'POST').opts.body),
    visible: n => elements['step-' + n].classList.contains('visible'),
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
    assert.equal(rig.requests.some(r => r.opts?.method === 'POST'), false);
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
    assert.equal(rig.requests.some(r => r.opts?.method === 'POST'), false);
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
      assert.equal(rig.requests.some(r => r.opts?.method === 'POST'), false);
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
    rig.go(3); rig.go(2); rig.go(8); rig.save();
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
    assert.equal(rig.requests.some(r => r.opts?.method === 'POST'), false);
  });

  it('keeps the Swedish fallback usable if the country lookup fails', async () => {
    const rig = wizard('?step=2', true);
    await settled();
    rig.elements['fuse-amps'].value = '16';
    rig.save();
    assert.equal(rig.posted().price.zone, 'SE3');
    assert.deepEqual(rig.posted().fuse, { phases: 3, voltage: 230, max_amps: 16 });
  });
});
