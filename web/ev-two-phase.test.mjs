import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const manual = source.slice(source.indexOf('function buildManualChargeSection'), source.indexOf('function sliderHeader'));

test('two-phase manual charging sends the displayed amps on two phases', async () => {
  const nodes = [];
  const document = { createElement(tag) {
    const node = { tag, style: {}, children: [], listeners: {},
      appendChild(child) { this.children.push(child); },
      cloneNode() { return document.createElement(tag); },
      setAttribute() {},
      addEventListener(name, fn) { this.listeners[name] = fn; },
    };
    nodes.push(node);
    return node;
  } };
  const writes = [];
  const build = new Function('document', 'evIsPaused', 'manualStatusText', 'evWrite', 'refreshEvModalAfterWrite',
    manual + '; return buildManualChargeSection;')(
    document, () => false, () => '', async (path, options) => {
      writes.push({ path, body: JSON.parse(options.body) });
      return { ok: true };
    }, () => {},
  );
  build({ id: 'garage', phases: 2, voltage_v: 230, min_charge_w: 1380, max_charge_w: 6400 });
  const slider = nodes.find(n => n.tag === 'input');
  assert.equal(slider.min, '6');
  assert.equal(slider.max, '13');
  slider.value = '10';
  slider.listeners.input();
  assert.ok(nodes.some(n => n.textContent === '10 A · 4.6 kW'));
  nodes.find(n => n.textContent === 'Charge now').listeners.click();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(writes, [{ path: '/api/loadpoints/garage/manual_hold', body: { power_w: 4600, hold_s: 0, phase_mode: '2p' } }]);
});
