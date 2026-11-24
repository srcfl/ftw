// A driver's DRIVER.config_secrets keys reach the setup wizard and the
// Devices tab in settings, and the settings tabs put many other values,
// some reported by devices, inside quoted attributes. These tests run that
// code with a value that tries to leave its attribute (#1142).

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const setup = readFileSync(new URL('./setup.js', import.meta.url), 'utf8');
const settings = readFileSync(new URL('./settings.js', import.meta.url), 'utf8');
const devices = readFileSync(new URL('./settings/tabs/devices.js', import.meta.url), 'utf8');

const HOSTILE = 'token" autofocus onfocus="alert(1)';

// What a browser does when it serializes a text node: only &, <, > and
// no-break space are escaped. Quotes are left as they are.
function serializeText(s) {
  return s.replace(/&/g, '&amp;').replace(/ /g, '&nbsp;')
    .replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function fakeElement(tag) {
  return {
    tag,
    attrs: {},
    children: [],
    markup: [],
    _text: '',
    set textContent(v) { this._text = String(v); },
    get textContent() { return this._text; },
    set innerHTML(v) { this.markup.push(v); },
    get innerHTML() { return serializeText(this._text); },
    appendChild(child) { this.children.push(child); return child; },
    setAttribute(name, value) { this.attrs[name] = String(value); },
  };
}

test('setup wizard builds secret inputs as nodes, not markup from the key', () => {
  const start = setup.indexOf("var secretsGroup = document.getElementById('drv-secrets-group');");
  assert.ok(start >= 0, 'the secrets block must exist');
  const loop = setup.indexOf('secrets.forEach(', start);
  const end = setup.indexOf('\n    });', loop);
  assert.ok(loop > start && end > loop, 'the secrets loop must exist');
  const block = setup.slice(start, end + '\n    });'.length);

  const group = fakeElement('div');
  const made = [];
  const document = {
    getElementById: (id) => (id === 'drv-secrets-group' ? group : null),
    createElement: (tag) => { const el = fakeElement(tag); made.push(el); return el; },
  };
  const esc = (s) => serializeText(String(s));
  vm.runInNewContext(block, { document, esc, selectedCatalog: { config_secrets: [HOSTILE] } });

  for (const el of [group, ...made]) {
    for (const html of el.markup) {
      assert.ok(!html.includes('onfocus'), 'the key must not be written as markup: ' + html);
    }
  }
  const input = made.find((el) => el.tag === 'input');
  const label = made.find((el) => el.tag === 'label');
  assert.ok(input && label, 'a label and an input must be made for the key');
  assert.equal(input.id, 'drv-secret-' + HOSTILE, 'saveDriver reads the input back by this id');
  assert.equal(input.type, 'password');
  assert.equal(label.htmlFor, input.id);
  assert.equal(group.children.length, 1);
});

test('settings escHtml is safe inside a quoted attribute', () => {
  const match = settings.match(/function escHtml\(s\) \{[\s\S]*?\n  \}/);
  assert.ok(match, 'settings.js must define escHtml');
  const document = { createElement: (tag) => fakeElement(tag) };
  const escHtml = vm.runInNewContext('(' + match[0] + ')', { document });

  const out = escHtml(HOSTILE);
  assert.ok(!out.includes('"'), 'a double quote must be escaped: ' + out);
  assert.ok(!out.includes("'"), 'a single quote must be escaped: ' + out);
  assert.equal(escHtml('<b>&</b>'), '&lt;b&gt;&amp;&lt;/b&gt;');
  assert.equal(escHtml(null), '');
});

test('the Devices tab passes the secret key through escHtml', () => {
  assert.ok(/'data-path="drivers\.' \+ dIdx \+ '\.config\.' \+ escHtml\(key\) \+ '"/.test(devices),
    'the data-path attribute must escape the key');
});
