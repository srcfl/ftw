// bootstrap-enroll.js — the home.* first-enrollment courier helpers.
//
// Multi-tenant onboarding (R-series rework) replaces the typed 6-digit PIN as the
// relay's claim handle with a HIGH-ENTROPY bootstrap_id carried in the URL
// fragment (#b=<id>). The fragment NEVER hits the network (browsers don't send it
// to servers), so the secret stays on the device; the browser derives the relay's
// claim handle from it as claimKey = hex(sha256(bootstrap_id)) and sends only that
// digest to the relay. The relay keys its blind bootstrap store on the same digest
// (cmd/ftw-relay/bootstrap.go) and NEVER sees the raw bootstrap_id.
//
// The 6-digit PIN STAYS as the optional manual second factor: it rides through the
// relay's enroll-forward UNTOUCHED (?pin=) and is validated by the Pi
// (ownerAccessState.validateEnrollPin, 5-try burn) — never by the relay.
//
// VERIFY BEFORE TRUST: the descriptor the relay hands back is Pi-signed cleartext
// the relay stores blind. claimAndVerify gates on verifyEntry (the Pi's ES256
// signature) and ABORTS if it fails, so a tampering relay cannot inject a fake
// instance even though it brokers the claim.
//
// ES-module exports below; via <script src> it also sets window.ftwBootstrapEnroll.

import { verifyEntry } from "./instance-sync.js";

// bootstrapIdFromHash extracts the bootstrap_id from a location.hash like
// "#b=<id>". Returns the raw id string, or null when absent/empty. Tolerates a
// leading "#", multiple "&"-joined params, and percent-encoding.
export function bootstrapIdFromHash(hash) {
  if (typeof hash !== "string") return null;
  const h = hash.charAt(0) === "#" ? hash.slice(1) : hash;
  if (!h) return null;
  for (const part of h.split("&")) {
    const eq = part.indexOf("=");
    if (eq < 0) continue;
    if (part.slice(0, eq) !== "b") continue;
    let v = part.slice(eq + 1);
    try { v = decodeURIComponent(v); } catch { /* keep raw on bad escape */ }
    return v.length ? v : null;
  }
  return null;
}

// hex lower-cases a byte buffer to a hex string.
function toHex(buf) {
  const u = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let s = "";
  for (let i = 0; i < u.length; i++) s += u[i].toString(16).padStart(2, "0");
  return s;
}

// claimKeyFromBootstrapId derives the relay's claim handle from the raw
// bootstrap_id: claimKey = hex(sha256(bootstrap_id)). MUST match the Pi
// (go/internal/api/bootstrap_publish.go) and the relay (cmd/ftw-relay) which key
// the store on the SAME digest — the relay never sees the raw secret. The input is
// hashed as its UTF-8 bytes (the Pi hashes the same base64url-no-pad string bytes).
export async function claimKeyFromBootstrapId(bootstrapId) {
  const bytes = new TextEncoder().encode(String(bootstrapId));
  const digest = await globalThis.crypto.subtle.digest("SHA-256", bytes);
  return toHex(digest);
}

// bootstrapProof is the ceremony-bound proof-of-possession of the RAW bootstrap_id:
//   bootstrap_proof = hex(HMAC-SHA256(key = utf8(bootstrap_id), msg = utf8(ceremony_token)))
// The browser sends it as ?bootstrap_proof=<hex> on enroll/FINISH so the Pi can
// prove the finisher actually holds the secret bootstrap_id that opened THIS
// ceremony. The relay only ever holds hex(sha256(bootstrap_id)) (the claim_key
// digest), so it can neither forge this proof for its own ceremony_token nor reuse
// the user's (the ceremony_token is single-use). MUST stay byte-identical to the Pi
// (go/internal/api/api_owner_access.go bootstrapEnrollProof): key = utf8 bytes of
// the bootstrap_id, message = utf8 bytes of the ceremony_token, lowercase-hex out.
export async function bootstrapProof(bootstrapId, ceremonyToken) {
  const enc = new TextEncoder();
  const key = await globalThis.crypto.subtle.importKey(
    "raw",
    enc.encode(String(bootstrapId)),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const mac = await globalThis.crypto.subtle.sign("HMAC", key, enc.encode(String(ceremonyToken)));
  return toHex(mac);
}

// claimAndVerify POSTs {home}/bootstrap/claim {claim_key} and returns the
// VERIFIED, parsed entry: { site_id, pi_pubkey, label, sig } merged with the
// relay-reported site_id. It performs the verify-before-trust gate:
//   - a non-200 claim → throws (no live bootstrap window for this id).
//   - a descriptor whose Pi ES256 signature fails verifyEntry → throws (a
//     tampering relay must not be able to inject a fake instance).
// `fetchImpl` defaults to globalThis.fetch (injectable for tests).
export async function claimAndVerify(homeBase, claimKey, fetchImpl) {
  const f = fetchImpl || globalThis.fetch;
  const url = String(homeBase).replace(/\/$/, "") + "/bootstrap/claim";
  const r = await f(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ claim_key: claimKey }),
  });
  if (r.status === 404) throw new Error("no live setup link — the PIN/QR may have expired; mint a fresh one on your home network");
  if (!r.ok) throw new Error("claim failed (" + r.status + ")");
  const { site_id, descriptor } = await r.json();
  if (!site_id || !descriptor) throw new Error("relay returned an incomplete claim");
  let entry;
  try {
    entry = JSON.parse(descriptor);
  } catch {
    throw new Error("relay returned a malformed descriptor");
  }
  // The relay stores the descriptor BLIND. Trust ONLY the Pi's signature over it.
  if (!(await verifyEntry(entry))) {
    throw new Error("descriptor signature did not verify — refusing to trust the relay");
  }
  // The relay-reported site_id must match the signed-over one (defence in depth:
  // verifyEntry already binds site_id into the signed message).
  if (entry.site_id !== site_id) {
    throw new Error("descriptor site_id mismatch — refusing to trust the relay");
  }
  return entry;
}

if (typeof window !== "undefined") {
  window.ftwBootstrapEnroll = { bootstrapIdFromHash, claimKeyFromBootstrapId, bootstrapProof, claimAndVerify };
}
