// device-key.js — silent device-key store for the home route (Phase 5).
//
// WHAT THIS IS
// A per-device, NON-EXTRACTABLE ECDSA P-256 keypair minted at LAN enrollment,
// persisted in IndexedDB keyed by ORIGIN, and used afterwards to:
//   - prove this device to the RELAY before posting a WebRTC offer (C2), so the
//     relay only ever wakes the Pi for a known device — never a stranger; and
//   - prove the device to the PI over the open channel to mint the owner session
//     SILENTLY (C3) — no Face ID for a returning device.
//
// WHY NON-EXTRACTABLE
// The private key never leaves the browser's key store. A compromised page can
// ask it to SIGN (the same trust boundary as a passkey) but can never exfiltrate
// it — so a leaked key can't be replayed from another machine. The matching
// public key is pinned on the Pi at LAN enrollment (C4) and published to the
// relay by the Pi (C1); only those two parties accept this device's signatures.
//
// WIRE FORMATS (must match the relay + Pi byte-for-byte — see CONTRACTS):
//   - pubHex: uncompressed P-256 public key as X||Y, 128 lowercase hex chars
//     (same convention as the nova site pubkey). NO 0x04 prefix in the hex.
//   - signatures: ECDSA P-256 over SHA-256, raw r||s (64 bytes), base64url, no
//     padding. WebCrypto ECDSA 'P-256' already produces raw r||s, so we only
//     base64url-encode it. Go verifies by splitting the 64 bytes into r,s.
//
// Importable as a module (ESM) AND usable as a classic script: when loaded via
// <script src> it also assigns window.ftwDeviceKey = { getOrCreate, exportPubHex,
// hasDeviceKey }. next-app.js (a classic IIFE) consumes the window global; the
// owner-access ceremony pages (ESM) import it.

import { bufToB64url } from "./webauthn.js";

const DB_NAME = "ftw-device-key";
const STORE = "keys";
// Keyed by ORIGIN so a key minted on one home origin can't be used as another's.
// location.origin already distinguishes scheme+host+port.
function recordKey() {
  try {
    return "device-key:" + (location.origin || "");
  } catch (_) {
    return "device-key:";
  }
}

// ---- IndexedDB plumbing (promise-wrapped) ---------------------------------

function openDB() {
  return new Promise((resolve, reject) => {
    let req;
    try {
      req = indexedDB.open(DB_NAME, 1);
    } catch (e) {
      reject(e);
      return;
    }
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE);
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error || new Error("indexedDB open failed"));
  });
}

function idbGet(key) {
  return openDB().then(
    (db) =>
      new Promise((resolve, reject) => {
        const tx = db.transaction(STORE, "readonly");
        const req = tx.objectStore(STORE).get(key);
        req.onsuccess = () => resolve(req.result || null);
        req.onerror = () => reject(req.error || new Error("idb get failed"));
      }),
  );
}

function idbPut(key, value) {
  return openDB().then(
    (db) =>
      new Promise((resolve, reject) => {
        const tx = db.transaction(STORE, "readwrite");
        const req = tx.objectStore(STORE).put(value, key);
        req.onsuccess = () => resolve(true);
        req.onerror = () => reject(req.error || new Error("idb put failed"));
      }),
  );
}

// ---- key material ----------------------------------------------------------

// rawXYToHex turns WebCrypto's "raw" EC public key (0x04 || X || Y, 65 bytes)
// into the 128-hex X||Y the relay + Pi expect. Drops the 0x04 lead byte.
function rawXYToHex(rawBuf) {
  const b = new Uint8Array(rawBuf);
  if (b.length !== 65 || b[0] !== 0x04) {
    throw new Error("unexpected EC raw public key length/format");
  }
  let hex = "";
  for (let i = 1; i < b.length; i++) hex += (b[i] + 0x100).toString(16).slice(1);
  return hex; // 128 lowercase hex chars
}

// makeHandle wraps a stored {privateKey, publicKey, pubHex} into the public
// surface getOrCreate() resolves to: pubHex + sign().
function makeHandle(rec) {
  return {
    pubHex: rec.pubHex,
    // sign(utf8string) -> base64url(r||s) over SHA-256, matching the Go verifier.
    sign(message) {
      const data =
        typeof message === "string"
          ? new TextEncoder().encode(message)
          : new Uint8Array(message);
      return crypto.subtle
        .sign({ name: "ECDSA", hash: "SHA-256" }, rec.privateKey, data)
        .then((sig) => {
          // WebCrypto returns raw r||s (64 bytes for P-256) — base64url it.
          if (sig.byteLength !== 64) {
            // Defensive: a non-64 length means a non-raw encoding slipped in;
            // never silently sign with an off-contract format.
            throw new Error("unexpected ECDSA signature length " + sig.byteLength);
          }
          return bufToB64url(sig);
        });
    },
  };
}

let _cache = null; // in-memory handle, deduped per page load

// getOrCreate resolves to { pubHex, sign(utf8)->Promise<b64url> }. It reuses the
// per-origin key from IndexedDB if present, else mints a fresh NON-EXTRACTABLE
// keypair, persists the CryptoKey pair (browsers can structured-clone a
// non-extractable CryptoKey into IndexedDB; the bytes still never leave the
// store), and returns the handle. Throws only on a hard crypto/IndexedDB failure
// — callers MUST surface a clear "set up this device first" state rather than
// silently proceeding.
export function getOrCreate() {
  if (_cache) return Promise.resolve(_cache);
  const key = recordKey();
  return idbGet(key)
    .then((stored) => {
      if (stored && stored.privateKey && stored.publicKey && stored.pubHex) {
        return makeHandle(stored);
      }
      // Mint a fresh non-extractable keypair. extractable=false → the private key
      // can never be exported; only sign() works. ["sign"] usage on the pair; the
      // public key is exported as "raw" for pubHex (a public key is exportable
      // regardless of the private key's extractable flag).
      return crypto.subtle
        .generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"])
        .then((kp) =>
          crypto.subtle.exportKey("raw", kp.publicKey).then((raw) => {
            const pubHex = rawXYToHex(raw);
            const rec = { privateKey: kp.privateKey, publicKey: kp.publicKey, pubHex };
            // Persist; if the put fails we still return a usable in-memory handle
            // for this load, but log so a broken store is visible.
            return idbPut(key, rec)
              .catch((e) => {
                try {
                  console.warn("device-key: persist failed: " + (e && e.message));
                } catch (_) {}
              })
              .then(() => makeHandle(rec));
          }),
        );
    })
    .then((handle) => {
      _cache = handle;
      return handle;
    });
}

// exportPubHex resolves the 128-hex public key (minting the key if needed). This
// is what enroll/finish posts as "device_pubkey" so the Pi can pin it (C4).
export function exportPubHex() {
  return getOrCreate().then((h) => h.pubHex);
}

// hasDeviceKey resolves true iff a key already exists for THIS origin WITHOUT
// minting one. The sign-in gate (C2/C3) uses it to decide between the silent
// device path and the "set up this device first" prompt — minting here would be
// wrong (a never-enrolled device would look enrolled to the relay, which has not
// pinned the freshly-minted key).
export function hasDeviceKey() {
  if (_cache) return Promise.resolve(true);
  return idbGet(recordKey())
    .then((stored) => !!(stored && stored.privateKey && stored.publicKey && stored.pubHex))
    .catch(() => false);
}

// Classic-script bridge for next-app.js (a plain IIFE that can't `import`).
try {
  if (typeof window !== "undefined") {
    window.ftwDeviceKey = { getOrCreate, exportPubHex, hasDeviceKey };
  }
} catch (_) {
  /* non-browser (test harness) — ignore */
}
