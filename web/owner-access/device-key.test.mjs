// node --test web/owner-access/device-key.test.mjs
//
// device-key.js mints + persists a NON-EXTRACTABLE ECDSA P-256 keypair per origin
// and exposes getOrCreate / exportPubHex / hasDeviceKey. These tests exercise the
// REAL WebCrypto path (node's globalThis.crypto.subtle) against an in-memory
// IndexedDB shim, and lock in the wire contract the relay + Pi verify against:
//   - pubHex: uncompressed X||Y, 128 lowercase hex chars, NO 0x04 prefix.
//   - sign(utf8) -> base64url(raw r||s, 64 bytes), no padding — verifiable as a
//     standard ECDSA P-256/SHA-256 signature (which is exactly what Go's
//     ecdsa.Verify over the split r||s checks).

import { describe, it, before, beforeEach } from "node:test";
import assert from "node:assert/strict";

// --- minimal in-memory IndexedDB shim --------------------------------------
// Just enough of the IndexedDB surface device-key.js uses: open(name) ->
// request{onupgradeneeded,onsuccess,result}, db.transaction(store,mode),
// store.get(key)/put(value,key) -> request{onsuccess,onerror,result}.
function makeIndexedDBShim() {
  const data = new Map(); // store-name -> Map(key->value)
  function reqFire(req, fn) {
    queueMicrotask(() => {
      try {
        req.result = fn();
        if (req.onsuccess) req.onsuccess();
      } catch (e) {
        req.error = e;
        if (req.onerror) req.onerror();
      }
    });
  }
  return {
    _data: data,
    open() {
      const db = {
        objectStoreNames: { contains: (n) => data.has(n) },
        createObjectStore: (n) => { if (!data.has(n)) data.set(n, new Map()); },
        transaction: (name) => ({
          objectStore: (n) => ({
            get(key) { const r = {}; reqFire(r, () => data.get(n)?.get(key) ?? undefined); return r; },
            put(value, key) { const r = {}; reqFire(r, () => { if (!data.has(n)) data.set(n, new Map()); data.get(n).set(key, value); return true; }); return r; },
          }),
        }),
      };
      const req = { result: db };
      queueMicrotask(() => {
        // schema upgrade first (v1), then success
        if (req.onupgradeneeded) req.onupgradeneeded();
        if (req.onsuccess) req.onsuccess();
      });
      return req;
    },
  };
}

// base64url -> Uint8Array (mirror of webauthn.js b64urlToBuf, returns bytes)
function b64urlToBytes(s) {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
  const bin = Buffer.from(b64, "base64");
  return new Uint8Array(bin);
}
function hexToBytes(h) {
  const a = new Uint8Array(h.length >> 1);
  for (let i = 0; i < a.length; i++) a[i] = parseInt(h.substr(i * 2, 2), 16);
  return a;
}

let deviceKey;

before(async () => {
  globalThis.location = { origin: "https://home.example.com", pathname: "/" };
  globalThis.window = globalThis.window || {};
  globalThis.indexedDB = makeIndexedDBShim();
  deviceKey = await import("./device-key.js?fresh=" + Date.now());
});

describe("device-key store (C2/C3/C4 key material)", () => {
  it("getOrCreate resolves a handle with a 128-hex pubHex and sign()", async () => {
    const h = await deviceKey.getOrCreate();
    assert.equal(typeof h.sign, "function");
    assert.match(h.pubHex, /^[0-9a-f]{128}$/, "pubHex must be 128 lowercase hex chars (X||Y, no 0x04)");
  });

  it("exportPubHex matches getOrCreate().pubHex and is stable across calls", async () => {
    const a = await deviceKey.exportPubHex();
    const b = await deviceKey.exportPubHex();
    const h = await deviceKey.getOrCreate();
    assert.equal(a, b, "same key reused → same pubHex");
    assert.equal(a, h.pubHex);
  });

  it("sign(utf8) returns base64url(raw r||s) of 64 bytes — the wire contract", async () => {
    const h = await deviceKey.getOrCreate();
    const sig = await h.sign("ftw-signal:v1:site:Home:abc123");
    assert.match(sig, /^[A-Za-z0-9_-]+$/, "base64url, no padding (no '=', no '+', no '/')");
    const raw = b64urlToBytes(sig);
    assert.equal(raw.length, 64, "P-256 raw r||s is exactly 64 bytes");
  });

  it("the signature verifies as standard ECDSA P-256/SHA-256 (Go ecdsa.Verify compatible)", async () => {
    const h = await deviceKey.getOrCreate();
    const message = "ftw-device-pop:v1:site:Home:challengeXYZ";
    const sig = b64urlToBytes(await h.sign(message));
    // Re-import the public key from pubHex exactly as Go would (X||Y) and verify
    // the raw r||s signature — proving the bytes are a real ECDSA signature, which
    // is what the Pi/relay (Go: split 64 bytes into r,s, ecdsa.Verify) accept.
    const xy = hexToBytes(h.pubHex);
    const raw = new Uint8Array(65);
    raw[0] = 0x04;
    raw.set(xy, 1);
    const pub = await crypto.subtle.importKey(
      "raw", raw.buffer, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"],
    );
    const ok = await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" }, pub, sig, new TextEncoder().encode(message),
    );
    assert.equal(ok, true, "signature must verify against the published pubHex");
  });

  it("hasDeviceKey reports true once a key exists, without re-minting a new one", async () => {
    const before = await deviceKey.exportPubHex();
    assert.equal(await deviceKey.hasDeviceKey(), true);
    const after = await deviceKey.exportPubHex();
    assert.equal(after, before, "hasDeviceKey must not rotate the key");
  });

  it("exposes window.ftwDeviceKey for the classic-script consumers (p2p.js/next-app.js)", () => {
    assert.equal(typeof globalThis.window.ftwDeviceKey, "object");
    assert.equal(typeof globalThis.window.ftwDeviceKey.getOrCreate, "function");
    assert.equal(typeof globalThis.window.ftwDeviceKey.exportPubHex, "function");
    assert.equal(typeof globalThis.window.ftwDeviceKey.hasDeviceKey, "function");
  });
});

describe("hasDeviceKey on a FRESH origin does not mint (C2 unenrolled detection)", () => {
  let fresh;
  beforeEach(async () => {
    // New origin + new IndexedDB → no stored key. hasDeviceKey() must stay false.
    globalThis.location = { origin: "https://fresh.example.com", pathname: "/" };
    globalThis.indexedDB = makeIndexedDBShim();
    fresh = await import("./device-key.js?fresh2=" + Date.now());
  });
  it("returns false when nothing is stored (and persists nothing)", async () => {
    assert.equal(await fresh.hasDeviceKey(), false, "a never-enrolled origin is unenrolled");
    // Confirm the probe did NOT create a key store entry.
    const store = globalThis.indexedDB._data.get("keys");
    assert.ok(!store || store.size === 0, "hasDeviceKey must not write");
  });
});
