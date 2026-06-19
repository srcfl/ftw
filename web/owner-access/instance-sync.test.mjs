import { test } from "node:test";
import assert from "node:assert/strict";

const subtle = globalThis.crypto.subtle;
const enc = new TextEncoder();

// ---- minimal localStorage + a FAITHFUL in-JS relay that mirrors the Go
// WalletBlobStore write-auth (Ed25519 verify over the canonical message +
// TOFU-pinned write_pub + bounded version monotonicity). ----
globalThis.localStorage = (() => {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
  };
})();

function b64stdToBytes(s) {
  const bin = atob(s);
  const u = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
  return u;
}
function bytesToB64url(b) {
  let s = "";
  const u = new Uint8Array(b);
  for (let i = 0; i < u.length; i++) s += String.fromCharCode(u[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
function toHex(b) {
  const u = new Uint8Array(b);
  let s = "";
  for (let i = 0; i < u.length; i++) s += u[i].toString(16).padStart(2, "0");
  return s;
}

const relay = { blobs: new Map() }; // W -> { ct, nonce, version, pubRaw }

async function relayVerify(W, version, nonceB64, ctB64, pubB64, sigB64) {
  const nonce = b64stdToBytes(nonceB64);
  const ct = b64stdToBytes(ctB64);
  const pubRaw = b64stdToBytes(pubB64);
  const sig = b64stdToBytes(sigB64);
  const hash = new Uint8Array(await subtle.digest("SHA-256", ct));
  const msg = enc.encode(
    "ftw-blob:v1:" + W + ":" + version + ":" + bytesToB64url(nonce) + ":" + toHex(hash),
  );
  const pub = await subtle.importKey("raw", pubRaw, { name: "Ed25519" }, false, ["verify"]);
  return { ok: await subtle.verify({ name: "Ed25519" }, pub, sig, msg), pubRaw, sig };
}

globalThis.fetch = async (url, opts = {}) => {
  const m = String(url).match(/\/wallet\/([^/]+)\/blob$/);
  const W = m ? m[1] : "";
  if (!opts.method || opts.method === "GET") {
    const b = relay.blobs.get(W);
    if (!b) return { status: 404, ok: false, json: async () => ({}) };
    return { status: 200, ok: true, json: async () => ({ ciphertext: b.ct, nonce: b.nonce, version: b.version }) };
  }
  // PUT
  const body = JSON.parse(opts.body);
  const v = await relayVerify(W, body.version, body.nonce, body.ciphertext, body.write_pub, body.sig);
  if (!v.ok) return { status: 403, ok: false, json: async () => ({}) };
  const prev = relay.blobs.get(W);
  if (prev) {
    if (toHex(prev.pubRaw) !== toHex(v.pubRaw)) return { status: 403, ok: false, json: async () => ({}) }; // TOFU pin
    if (body.version <= prev.version) return { status: 409, ok: false, json: async () => ({}) };
  } else if (body.version <= 0) {
    return { status: 409, ok: false, json: async () => ({}) };
  }
  relay.blobs.set(W, { ct: body.ciphertext, nonce: body.nonce, version: body.version, pubRaw: v.pubRaw });
  return { status: 200, ok: true, json: async () => ({}) };
};

const { loadDirectory, saveDirectory, verifyEntry, getCachedInstances } = await import("./instance-sync.js");
const { deriveEncKey } = await import("./prf.js");

const W = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"; // 43-char base64url
const encKey = await deriveEncKey(new Uint8Array(32).fill(0x11).buffer);

// Build a Pi-signed instance descriptor (ES256 over the canonical message).
async function signedInstance(label) {
  const kp = await subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const rawPub = new Uint8Array(await subtle.exportKey("raw", kp.publicKey)); // 0x04||X||Y
  const piPubHex = toHex(rawPub.slice(1));
  const site_id = "site:" + label;
  const msg = enc.encode("ftw-instance:v1:" + site_id + ":" + piPubHex + ":" + label);
  const sig = new Uint8Array(await subtle.sign({ name: "ECDSA", hash: "SHA-256" }, kp.privateKey, msg));
  return { site_id, pi_pubkey: piPubHex, label, sig: bytesToB64url(sig), added_ms: 1 };
}

test("verifyEntry accepts a Pi-signed descriptor and rejects tampering", async () => {
  const e = await signedInstance("Home");
  assert.ok(await verifyEntry(e));
  assert.equal(await verifyEntry({ ...e, label: "Evil" }), false); // label not signed-over
  assert.equal(await verifyEntry({ ...e, sig: bytesToB64url(new Uint8Array(64)) }), false);
});

test("save then load round-trips the directory through the relay (writer-authed)", async () => {
  relay.blobs.clear();
  const home = await signedInstance("Home");
  // First write: brand-new wallet, generates the Ed25519 write key, stores it in the blob.
  const saved = await saveDirectory(W, encKey, "https://relay.test", { instances: [home], version: 0, writeKey: null });
  assert.equal(saved.putStatus, 200, "first PUT must be accepted by the relay");

  // A FRESH "device": clear the browser-carried cache, then load purely from the relay blob.
  localStorage.removeItem("ftw.directory");
  const loaded = await loadDirectory(W, encKey, "https://relay.test");
  assert.ok(loaded, "fresh device must recover the directory from the relay");
  assert.equal(loaded.instances.length, 1);
  assert.equal(loaded.instances[0].site_id, "site:Home");
  assert.ok(loaded.writeKey, "fresh device must recover the write key from the blob");

  // Update from the fresh device using the recovered write key.
  const lab = await signedInstance("Cabin");
  const merged = { instances: [...loaded.instances, lab], version: loaded.version, writeKey: loaded.writeKey };
  const saved2 = await saveDirectory(W, encKey, "https://relay.test", merged);
  assert.equal(saved2.putStatus, 200, "update with the pinned key must be accepted");
  assert.equal(getCachedInstances().length, 2);
});

test("a wrong PRF key cannot decrypt the blob and degrades to the cache", async () => {
  const wrong = await deriveEncKey(new Uint8Array(32).fill(0x99).buffer);
  localStorage.removeItem("ftw.directory"); // no cache
  const loaded = await loadDirectory(W, wrong, "https://relay.test");
  assert.equal(loaded, null, "undecryptable blob + empty cache → null (no homes leaked)");
});

test("a stranger with a different write key is rejected (403) — no takeover", async () => {
  // The relay already pinned the owner's key in the round-trip test above. A
  // brand-new writeKey for the same W must be refused.
  relay.blobs.clear();
  const home = await signedInstance("Home");
  await saveDirectory(W, encKey, "https://relay.test", { instances: [home], version: 0, writeKey: null });
  const attacker = await saveDirectory(W, encKey, "https://relay.test", { instances: [home], version: 0, writeKey: null });
  // attacker passes writeKey:null → genWriteKey() → different pub → relay 403 (and 409 retry also 403)
  assert.equal(attacker.putStatus, 403, "a different write key must be refused by the TOFU pin");
});
