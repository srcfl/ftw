// prf.js — derive the per-wallet directory ENCRYPTION key from a WebAuthn passkey
// via the PRF extension, for the multi-tenant home route.
//
// The same passkey the owner taps to sign in also yields, through the WebAuthn
// `prf` extension, a high-entropy secret that is STABLE for that credential. We
// HKDF it into a non-extractable AES-GCM-256 key and use that to encrypt/decrypt
// the wallet's directory blob (the list of the owner's 42W instances) that the
// relay stores opaquely. Because the key derives from the passkey — not from any
// device-local secret — a FRESH device with the SYNCED passkey derives the SAME
// key and can decrypt the blob it fetches from the relay, with no LAN visit.
//
// HONEST RESIDUAL: PRF output determinism across iCloud Keychain / Google Password
// Manager passkey SYNC is undocumented and must be verified on real devices before
// the relay blob is relied on for fresh-device bootstrap. Firefox ships no PRF at
// all. When PRF is unavailable, callers fall back to the browser-carried directory
// copy (instance-sync.js) and lose only remote fresh-device discovery.
//
// As an ES module it exports the functions below; loaded via a classic <script src>
// it also assigns window.ftwPrf = { extensionInput, outputFrom, supported, deriveEncKey }.

// FIXED_SALT_32 is the fixed 32-byte PRF eval input + HKDF salt. It is a public
// domain-separation constant (NOT a secret) — it only ensures this app's PRF
// evaluation and key derivation are distinct from any other use of the credential.
export const FIXED_SALT_32 = new Uint8Array([
  0x66, 0x74, 0x77, 0x2d, 0x68, 0x6f, 0x6d, 0x65, // "ftw-home"
  0x2d, 0x70, 0x72, 0x66, 0x2d, 0x76, 0x31, 0x2e, // "-prf-v1."
  0x9a, 0x3c, 0x71, 0xe5, 0x4d, 0x88, 0x0b, 0x12, // random tail
  0xf6, 0x27, 0x90, 0xab, 0x5e, 0xd4, 0x33, 0x7c,
]);

const HKDF_INFO = new TextEncoder().encode("ftw-instance-blob:aes-gcm:v1");

// extensionInput returns the `extensions` object to pass to
// navigator.credentials.get({ publicKey: { ..., extensions: extensionInput() } }).
// Requesting prf.eval.first makes the authenticator evaluate the PRF at our salt.
export function extensionInput() {
  return { prf: { eval: { first: FIXED_SALT_32.buffer.slice(0) } } };
}

// outputFrom extracts the raw PRF secret (ArrayBuffer, 32 bytes) from a completed
// assertion, or null when the authenticator/browser did not return one (no PRF
// support, or the credential was created without the prf extension enabled).
export function outputFrom(assertion) {
  try {
    const ext = assertion && assertion.getClientExtensionResults
      ? assertion.getClientExtensionResults()
      : null;
    const first = ext && ext.prf && ext.prf.results ? ext.prf.results.first : null;
    if (!first) return null;
    const buf = first instanceof ArrayBuffer ? first : (first.buffer || null);
    if (!buf || buf.byteLength < 32) return null;
    return buf.slice(0, 32);
  } catch {
    return null;
  }
}

// supported reports whether an assertion carried a usable PRF output.
export function supported(assertion) {
  return outputFrom(assertion) !== null;
}

// deriveEncKey HKDFs a PRF output into a NON-EXTRACTABLE AES-GCM-256 key bound to
// this app's domain (salt + info). The same prfOutput always yields the same key,
// so any device with the synced passkey can decrypt the directory blob.
export async function deriveEncKey(prfOutput) {
  const subtle = globalThis.crypto.subtle;
  const base = await subtle.importKey("raw", prfOutput, "HKDF", false, ["deriveKey"]);
  return subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: FIXED_SALT_32, info: HKDF_INFO },
    base,
    { name: "AES-GCM", length: 256 },
    false, // non-extractable
    ["encrypt", "decrypt"],
  );
}

if (typeof window !== "undefined") {
  window.ftwPrf = { extensionInput, outputFrom, supported, deriveEncKey, FIXED_SALT_32 };
}
