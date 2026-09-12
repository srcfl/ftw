import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const html = fs.readFileSync(new URL('./source.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

async function render(version) {
  const elements = Object.fromEntries(['build', 'source', 'license', 'instructions', 'drivers'].map(id => [id, {}]));
  vm.runInNewContext(script, {
    document: { getElementById: id => elements[id] },
    fetch: async () => ({ ok: true, json: async () => ({ version }) }),
  });
  await new Promise(resolve => setImmediate(resolve));
  return elements;
}

test('source offer pins an official release including beta tags', async () => {
  const elements = await render('3.4.3-beta.1');
  assert.equal(elements.source.href, 'https://github.com/srcfl/ftw/archive/refs/tags/v3.4.3-beta.1.tar.gz');
  assert.match(elements.license.href, /\/v3\.4\.3-beta\.1\/LICENSE$/);
});

test('source offer does not call a moving branch the source of a dirty or custom build', async () => {
  for (const version of ['dev', 'v3.4.3-dirty', 'v3.4.3-2-gabcdef1', '<script>']) {
    const elements = await render(version);
    assert.equal(elements.source.href, undefined);
    assert.match(elements.build.textContent, /obtain matching source/i);
  }
});
