import { test } from "node:test";
import assert from "node:assert/strict";
import { FIXED_SALT_32, extensionInput, outputFrom, supported, deriveEncKey } from "./prf.js";

const subtle = globalThis.crypto.subtle;

test("FIXED_SALT_32 is a stable 32-byte public constant", () => {
  assert.equal(FIXED_SALT_32.length, 32);
});

test("extensionInput requests prf.eval.first with the salt", () => {
  const ext = extensionInput();
  assert.ok(ext.prf && ext.prf.eval && ext.prf.eval.first);
  assert.equal(new Uint8Array(ext.prf.eval.first).length, 32);
});

test("outputFrom extracts a 32-byte PRF secret, or null when absent", () => {
  const prf = new Uint8Array(32).fill(7).buffer;
  const withPrf = { getClientExtensionResults: () => ({ prf: { results: { first: prf } } }) };
  assert.equal(outputFrom(withPrf).byteLength, 32);
  assert.ok(supported(withPrf));

  const noPrf = { getClientExtensionResults: () => ({}) };
  assert.equal(outputFrom(noPrf), null);
  assert.equal(supported(noPrf), false);
  // A short/garbage result is rejected (treated as unsupported).
  const shortPrf = { getClientExtensionResults: () => ({ prf: { results: { first: new Uint8Array(8).buffer } } }) };
  assert.equal(outputFrom(shortPrf), null);
});

test("deriveEncKey is deterministic and round-trips AES-GCM", async () => {
  const prf = new Uint8Array(32).fill(0x42).buffer;
  const k1 = await deriveEncKey(prf);
  const k2 = await deriveEncKey(prf.slice(0)); // same secret, different buffer
  const nonce = new Uint8Array(12).fill(1);
  const msg = new TextEncoder().encode("hello directory");
  const ct = await subtle.encrypt({ name: "AES-GCM", iv: nonce }, k1, msg);
  const pt = await subtle.decrypt({ name: "AES-GCM", iv: nonce }, k2, ct);
  assert.equal(new TextDecoder().decode(pt), "hello directory");
});

test("a different PRF secret yields a key that cannot decrypt", async () => {
  const a = await deriveEncKey(new Uint8Array(32).fill(1).buffer);
  const b = await deriveEncKey(new Uint8Array(32).fill(2).buffer);
  const nonce = new Uint8Array(12).fill(9);
  const ct = await subtle.encrypt({ name: "AES-GCM", iv: nonce }, a, new TextEncoder().encode("secret"));
  await assert.rejects(() => subtle.decrypt({ name: "AES-GCM", iv: nonce }, b, ct));
});

test("the derived key is non-extractable", async () => {
  const k = await deriveEncKey(new Uint8Array(32).fill(3).buffer);
  await assert.rejects(() => subtle.exportKey("raw", k));
});
