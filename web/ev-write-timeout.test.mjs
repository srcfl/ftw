import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const implementation = source.slice(source.indexOf('  function evWrite('), source.indexOf('  // ---- Chart data ----'));

test('a charger write that sends headers but stalls its body settles with an unconfirmed result', async () => {
  let expire;
  const write = new Function('apiFetch', 'setTimeout', 'clearTimeout', implementation + '; return evWrite;')(
    (_path, options) => Promise.resolve({
      arrayBuffer: () => new Promise((_resolve, reject) => {
        options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
      }),
    }),
    callback => { expire = callback; return 1; },
    () => {},
  );
  const request = write('/api/loadpoints/car/manual_hold', { method: 'POST' });
  await Promise.resolve();
  expire();
  await assert.rejects(request, /has not confirmed the request/);
});

test('a complete refusal keeps the server response for the control that sent it', async () => {
  const write = new Function('apiFetch', implementation + '; return evWrite;')(
    () => Promise.resolve(new Response(JSON.stringify({ error: 'Charger unavailable' }), { status: 409 })),
  );
  const response = await write('/api/loadpoints/car/manual_hold', { method: 'POST' });
  assert.equal(response.status, 409);
  assert.deepEqual(await response.json(), { error: 'Charger unavailable' });
});
