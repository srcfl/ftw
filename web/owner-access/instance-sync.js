// instance-sync.js — the per-wallet DIRECTORY manager for the multi-tenant home
// route. The directory is the list of a wallet's FTW instances
// [{site_id, pi_pubkey, label}], each entry SIGNED by its Pi. It lives in two
// places, by design (the "hybrid" binding):
//
//   1. Browser-carried copy (localStorage) — the SOURCE OF TRUTH for routing +
//      display. Works with no network and no PRF.
//   2. Relay-held CIPHERTEXT blob — the same list AES-GCM-encrypted under the
//      PRF-derived key (prf.js), keyed by the opaque WebAuthn userHandle. Lets a
//      FRESH device with the synced passkey fetch + decrypt its homes remotely.
//
// The relay never decrypts the blob. Writes are authenticated by an Ed25519 WRITE
// KEY that is generated once, kept INSIDE the encrypted blob (so it syncs to the
// owner's other devices), and whose public half the relay TOFU-pins — so a
// userHandle-knower who cannot decrypt the blob cannot forge a write. The signed
// PUT message is byte-identical to the relay's Go blobWriteMessage.
//
// ES module exports below; via <script src> it also sets window.ftwInstanceSync.

const subtle = globalThis.crypto.subtle;
const LS_DIR = "ftw.directory"; // browser-carried instances cache (no secrets)
const enc = new TextEncoder();
const dec = new TextDecoder();

// ---- encoding helpers ----
function bytesToB64std(b) {
  let s = "";
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  for (let i = 0; i < u.length; i++) s += String.fromCharCode(u[i]);
  return btoa(s);
}
function b64stdToBytes(s) {
  const bin = atob(s);
  const u = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
  return u;
}
function bytesToB64url(b) {
  return bytesToB64std(b).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
function b64urlToBytes(s) {
  return b64stdToBytes(s.replace(/-/g, "+").replace(/_/g, "/"));
}
function toHex(b) {
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  let s = "";
  for (let i = 0; i < u.length; i++) s += u[i].toString(16).padStart(2, "0");
  return s;
}
function hexToBytes(h) {
  const u = new Uint8Array(h.length / 2);
  for (let i = 0; i < u.length; i++) u[i] = parseInt(h.substr(i * 2, 2), 16);
  return u;
}

// ---- relay wire ----
function walletBlobURL(relayBase, userHandleB64u) {
  return `${relayBase.replace(/\/$/, "")}/wallet/${userHandleB64u}/blob`;
}
async function fetchBlob(relayBase, W) {
  const r = await fetch(walletBlobURL(relayBase, W), { cache: "no-store" });
  if (r.status === 404) return null;
  if (!r.ok) throw new Error("wallet blob GET " + r.status);
  return r.json(); // { ciphertext, nonce, version }
}
async function putBlob(relayBase, W, body) {
  const r = await fetch(walletBlobURL(relayBase, W), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return r.status; // 200 | 409 | 403 | 413 | 400 | 503
}

// ---- blob crypto ----
async function decryptBlob(encKey, blob) {
  const nonce = b64stdToBytes(blob.nonce);
  const ct = b64stdToBytes(blob.ciphertext);
  const pt = await subtle.decrypt({ name: "AES-GCM", iv: nonce }, encKey, ct);
  return JSON.parse(dec.decode(pt));
}
async function encryptDir(encKey, plaintextObj) {
  const nonce = globalThis.crypto.getRandomValues(new Uint8Array(12));
  const data = enc.encode(JSON.stringify(plaintextObj));
  const ct = new Uint8Array(await subtle.encrypt({ name: "AES-GCM", iv: nonce }, encKey, data));
  return { nonce, ciphertext: ct };
}

// blobWriteMessage MUST match the relay's Go implementation byte-for-byte:
//   "ftw-blob:v1:" + handle + ":" + version + ":" + base64url(nonce) + ":" + hex(sha256(ciphertext))
async function blobWriteMessage(W, version, nonce, ciphertext) {
  const hash = new Uint8Array(await subtle.digest("SHA-256", ciphertext));
  return enc.encode(
    "ftw-blob:v1:" + W + ":" + version + ":" + bytesToB64url(nonce) + ":" + toHex(hash),
  );
}

// ---- write key (Ed25519, generated once, stored in the encrypted blob) ----
async function genWriteKey() {
  const kp = await subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  return {
    priv: kp.privateKey,
    pubRaw: new Uint8Array(await subtle.exportKey("raw", kp.publicKey)),
    privPkcs8: new Uint8Array(await subtle.exportKey("pkcs8", kp.privateKey)),
  };
}
async function importWriteKey(privPkcs8, pubRaw) {
  return {
    priv: await subtle.importKey("pkcs8", privPkcs8, { name: "Ed25519" }, true, ["sign"]),
    pubRaw,
    privPkcs8,
  };
}
async function signWrite(writeKey, message) {
  return new Uint8Array(await subtle.sign({ name: "Ed25519" }, writeKey.priv, message));
}

// ---- per-entry Pi signature verification ----
// importP256Pub imports a Pi identity public key given as hex X||Y (128 chars).
async function importP256Pub(piPubHex) {
  const raw = new Uint8Array(65);
  raw[0] = 0x04; // uncompressed point
  raw.set(hexToBytes(piPubHex), 1);
  return subtle.importKey("raw", raw, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
}
function instanceMessage(e) {
  return enc.encode("ftw-instance:v1:" + e.site_id + ":" + e.pi_pubkey + ":" + e.label);
}
// verifyEntry checks the Pi's ES256 signature over the instance descriptor, so a
// tampering relay cannot inject a fake instance even though it stores the blob.
export async function verifyEntry(e) {
  try {
    if (!e || !e.site_id || !e.pi_pubkey || !e.sig || e.pi_pubkey.length !== 128) return false;
    const pub = await importP256Pub(e.pi_pubkey);
    const sig = b64urlToBytes(e.sig);
    if (sig.length !== 64) return false;
    return subtle.verify({ name: "ECDSA", hash: "SHA-256" }, pub, sig, instanceMessage(e));
  } catch {
    return false;
  }
}

// ---- browser-carried cache ----
export function getCachedInstances() {
  try {
    const raw = globalThis.localStorage ? localStorage.getItem(LS_DIR) : null;
    if (!raw) return [];
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? arr : [];
  } catch {
    return [];
  }
}
function cacheInstances(instances) {
  try {
    if (globalThis.localStorage) localStorage.setItem(LS_DIR, JSON.stringify(instances));
  } catch {
    /* private mode / quota — non-fatal */
  }
}
function mergeUnion(a, b) {
  const bySite = new Map();
  for (const e of [...(a || []), ...(b || [])]) {
    if (!e || !e.site_id) continue;
    const prev = bySite.get(e.site_id);
    if (!prev || (e.added_ms || 0) >= (prev.added_ms || 0)) bySite.set(e.site_id, e);
  }
  return [...bySite.values()];
}

// loadDirectory fetches + decrypts + verifies the wallet's directory from the
// relay, merges it with the browser-carried copy, and returns
// { instances, version, writeKey } — or null when there is no blob and no cache.
// encKey comes from prf.deriveEncKey(); pass null when PRF is unavailable to fall
// back to the browser-carried copy only.
export async function loadDirectory(userHandleB64u, encKey, relayBase) {
  const cached = getCachedInstances();
  if (!encKey) {
    return cached.length ? { instances: cached, version: 0, writeKey: null } : null;
  }
  let blob = null;
  try {
    blob = await fetchBlob(relayBase, userHandleB64u);
  } catch {
    blob = null; // network/relay error → fall back to cache
  }
  if (!blob) {
    return cached.length ? { instances: cached, version: 0, writeKey: null } : null;
  }
  let pt;
  try {
    pt = await decryptBlob(encKey, blob);
  } catch {
    // Decrypt failed (PRF mismatch across sync, or wrong/garbage blob). Degrade to
    // the browser-carried copy rather than losing the homes.
    return cached.length ? { instances: cached, version: 0, writeKey: null } : null;
  }
  const verified = [];
  for (const e of pt.instances || []) {
    if (await verifyEntry(e)) verified.push(e);
  }
  const instances = mergeUnion(cached, verified);
  cacheInstances(instances);
  let writeKey = null;
  if (pt.write_priv && pt.write_pub) {
    try {
      writeKey = await importWriteKey(b64urlToBytes(pt.write_priv), b64urlToBytes(pt.write_pub));
    } catch {
      writeKey = null;
    }
  }
  return { instances, version: blob.version | 0, writeKey };
}

// saveDirectory re-encrypts the directory and writes it to the relay, writer-
// authenticated. `dir` is the object from loadDirectory (or {instances:[],
// version:0, writeKey:null} for a brand-new wallet). It updates the browser copy
// regardless, then attempts the relay PUT when encKey is available; on a 409 it
// re-reads, merges, and retries once. Returns the updated dir.
export async function saveDirectory(userHandleB64u, encKey, relayBase, dir) {
  const instances = dir.instances || [];
  cacheInstances(instances); // browser-carried copy is the source of truth
  if (!encKey) return { ...dir, instances };

  let writeKey = dir.writeKey || (await genWriteKey());
  let version = (dir.version | 0) + 1;

  const attempt = async () => {
    const plaintext = {
      v: 1,
      write_priv: bytesToB64url(writeKey.privPkcs8),
      write_pub: bytesToB64url(writeKey.pubRaw),
      instances,
    };
    const { nonce, ciphertext } = await encryptDir(encKey, plaintext);
    const msg = await blobWriteMessage(userHandleB64u, version, nonce, ciphertext);
    const sig = await signWrite(writeKey, msg);
    return putBlob(relayBase, userHandleB64u, {
      ciphertext: bytesToB64std(ciphertext),
      nonce: bytesToB64std(nonce),
      version,
      write_pub: bytesToB64std(writeKey.pubRaw),
      sig: bytesToB64std(sig),
    });
  };

  let status = await attempt();
  if (status === 409) {
    // Lost-update: re-read the latest, merge, bump past it, retry once.
    const latest = await loadDirectory(userHandleB64u, encKey, relayBase);
    if (latest) {
      version = (latest.version | 0) + 1;
      if (latest.writeKey) writeKey = latest.writeKey; // keep the pinned key
      // instances already merged via cache by loadDirectory
    }
    status = await attempt();
  }
  return { instances, version, writeKey, putStatus: status };
}

if (typeof window !== "undefined") {
  window.ftwInstanceSync = { loadDirectory, saveDirectory, verifyEntry, getCachedInstances };
}
