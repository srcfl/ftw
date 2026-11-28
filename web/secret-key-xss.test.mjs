// A driver's DRIVER.config_secrets keys reach the setup wizard and the
// Devices tab in settings, and the settings tabs put many other values,
// some reported by devices, inside quoted attributes. These tests run that
// code with a value that tries to leave its attribute (#1142).

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

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

// The setup guide's secret inputs run in setup-connection.test.mjs, against
// the real guide with a hostile key.

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
