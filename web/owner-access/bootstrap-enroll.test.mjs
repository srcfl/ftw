import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// instance-sync.js (imported transitively by bootstrap-enroll.js → verifyEntry)
// touches localStorage at import-time guards; provide a minimal shim so the
// module loads under node, mirroring instance-sync.test.mjs.
globalThis.localStorage = (() => {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
  };
})();

const subtle = globalThis.crypto.subtle;
const enc = new TextEncoder();

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

const { bootstrapIdFromHash, claimKeyFromBootstrapId, claimAndVerify, bootstrapProof } =
  await import("./bootstrap-enroll.js");

// Build a Pi-signed instance descriptor (ES256 over the canonical message), the
// SAME shape go/internal/api/buildInstanceDescriptor emits and verifyEntry checks.
async function signedDescriptor(label, siteOverride) {
  const kp = await subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const rawPub = new Uint8Array(await subtle.exportKey("raw", kp.publicKey)); // 0x04||X||Y
  const piPubHex = toHex(rawPub.slice(1));
  const site_id = siteOverride || ("site:" + label);
  const msg = enc.encode("ftw-instance:v1:" + site_id + ":" + piPubHex + ":" + label);
  const sig = new Uint8Array(await subtle.sign({ name: "ECDSA", hash: "SHA-256" }, kp.privateKey, msg));
  return { site_id, entry: { site_id, pi_pubkey: piPubHex, label, sig: bytesToB64url(sig) } };
}

test("bootstrapIdFromHash parses #b=<id>, tolerating leading # and extra params", () => {
  assert.equal(bootstrapIdFromHash("#b=AbC-123_xyz"), "AbC-123_xyz");
  assert.equal(bootstrapIdFromHash("b=AbC-123_xyz"), "AbC-123_xyz"); // no leading #
  assert.equal(bootstrapIdFromHash("#foo=1&b=ZZZ&bar=2"), "ZZZ");
  assert.equal(bootstrapIdFromHash("#"), null);
  assert.equal(bootstrapIdFromHash(""), null);
  assert.equal(bootstrapIdFromHash("#b="), null);
  assert.equal(bootstrapIdFromHash("#pin=123456"), null); // no b= → null
  assert.equal(bootstrapIdFromHash(null), null);
});

test("claimKeyFromBootstrapId = hex(sha256(bootstrap_id)) — known vector", async () => {
  // Cross-checked against Go: sha256("ABC123") (relay + Pi key the store on this).
  assert.equal(
    await claimKeyFromBootstrapId("ABC123"),
    "e0bebd22819993425814866b62701e2919ea26f1370499c1037b53b9d49c2c8a",
  );
  // Output is always 64 lowercase-hex chars (the relay's isLowerHex64 gate shape).
  const ck = await claimKeyFromBootstrapId(bytesToB64url(new Uint8Array(32).fill(7)));
  assert.match(ck, /^[0-9a-f]{64}$/);
});

test("bootstrapProof = hex(HMAC-SHA256(key=bootstrap_id, msg=ceremony_token + '|' + sha256(body))) — Go-verified vector", async () => {
  // Cross-checked against Go bootstrapEnrollProof + openssl:
  //   BODY='{"id":"abc","device_pubkey":"deadbeef"}'
  //   HASH=$(printf '%s' "$BODY" | openssl dgst -sha256 -r | cut -d' ' -f1)
  //   printf '%s' "ceremony-token-XYZ|$HASH" | openssl dgst -sha256 -hmac 'the-raw-bootstrap-id'
  const body1 = '{"id":"abc","device_pubkey":"deadbeef"}';
  assert.equal(
    await bootstrapProof("the-raw-bootstrap-id", "ceremony-token-XYZ", body1),
    "315fef2626c2dc66e5bfc58851bef92f937f530e00d93238bf1f95e40781aba1",
  );
  // A second Go-verified vector (key="ABC123", msg ceremony="tok-1", body='{"x":1}').
  assert.equal(
    await bootstrapProof("ABC123", "tok-1", '{"x":1}'),
    "019aa77bc1bad1c8e7cf7fa5faafb72b58fe6a65650839708d3a131aac31e055",
  );
  // Output is always 64 lowercase-hex chars (matches Go hex.EncodeToString of a 32-byte MAC).
  assert.match(await bootstrapProof("any", "thing", "body"), /^[0-9a-f]{64}$/);
  // The proof is ceremony-bound: a different ceremony_token yields a different proof
  // (so a relay cannot reuse a captured proof for its own ceremony).
  assert.notEqual(
    await bootstrapProof("the-raw-bootstrap-id", "ceremony-token-XYZ", body1),
    await bootstrapProof("the-raw-bootstrap-id", "ceremony-token-OTHER", body1),
  );
});

test("bootstrapProof is BODY-bound — a swapped device_pubkey yields a different proof (closes the MITM-relay device_pubkey swap)", async () => {
  // The honest browser proves over the body it POSTs. A MITM relay that swaps the
  // top-level device_pubkey for its OWN key produces a DIFFERENT body, so the proof
  // the Pi recomputes over the received body no longer matches the forwarded proof.
  const honest = '{"id":"abc","device_pubkey":"deadbeef"}';
  const tampered = '{"id":"abc","device_pubkey":"feedface"}';
  const proofHonest = await bootstrapProof("the-raw-bootstrap-id", "ceremony-token-XYZ", honest);
  const proofTampered = await bootstrapProof("the-raw-bootstrap-id", "ceremony-token-XYZ", tampered);
  assert.notEqual(proofHonest, proofTampered);
  // Cross-checked against Go (diffbodyB): the relay can't recompute proofTampered
  // without the raw bootstrap_id, and proofHonest doesn't match the tampered body.
  assert.equal(proofTampered, "0dac0839816e7100a37e51598d9bd466f929179b7f3f76cd8b0c7d2025323ad9");
});

test("claimAndVerify accepts a Pi-signed descriptor (verify-before-trust passes)", async () => {
  const { site_id, entry } = await signedDescriptor("Home");
  const relay = async (url, opts) => {
    assert.match(String(url), /\/bootstrap\/claim$/);
    const body = JSON.parse(opts.body);
    assert.match(body.claim_key, /^[0-9a-f]{64}$/); // browser sends the DIGEST, not the id
    return { status: 200, ok: true, json: async () => ({ site_id, descriptor: JSON.stringify(entry) }) };
  };
  const ck = await claimKeyFromBootstrapId("the-raw-bootstrap-id");
  const got = await claimAndVerify("https://home.test", ck, relay);
  assert.equal(got.site_id, site_id);
  assert.equal(got.pi_pubkey, entry.pi_pubkey);
});

test("claimAndVerify ABORTS on a tampered descriptor (relay cannot inject a fake instance)", async () => {
  const { site_id, entry } = await signedDescriptor("Home");
  const tampered = { ...entry, pi_pubkey: "0".repeat(128) }; // swap the key → sig no longer verifies
  const relay = async () => ({ status: 200, ok: true, json: async () => ({ site_id, descriptor: JSON.stringify(tampered) }) });
  const ck = await claimKeyFromBootstrapId("id");
  await assert.rejects(() => claimAndVerify("https://home.test", ck, relay), /did not verify|incomplete|mismatch/i);
});

test("claimAndVerify ABORTS when the relay-reported site_id disagrees with the signed one", async () => {
  // A descriptor validly signed for site:Other, but the relay claims it is site:Home.
  const { entry } = await signedDescriptor("Other", "site:Other");
  const relay = async () => ({ status: 200, ok: true, json: async () => ({ site_id: "site:Home", descriptor: JSON.stringify(entry) }) });
  const ck = await claimKeyFromBootstrapId("id");
  await assert.rejects(() => claimAndVerify("https://home.test", ck, relay), /mismatch/i);
});

test("claimAndVerify surfaces a 404 (expired/absent window) cleanly", async () => {
  const relay = async () => ({ status: 404, ok: false, json: async () => ({}) });
  const ck = await claimKeyFromBootstrapId("id");
  await assert.rejects(() => claimAndVerify("https://home.test", ck, relay), /no longer live|fresh setup QR|single-use/i);
});

test("claimAndVerify preserves relay diagnostic codes on non-404 claim failures", async () => {
  const relay = async () => ({
    status: 429,
    ok: false,
    headers: { get: (name) => name === "X-FTW-Error-Code" ? "FTW_BOOTSTRAP_RATE_LIMITED" : "" },
    text: async () => "FTW_BOOTSTRAP_RATE_LIMITED: too many setup attempts",
  });
  const ck = await claimKeyFromBootstrapId("id");
  await assert.rejects(
    async () => claimAndVerify("https://home.test", ck, relay),
    (err) => err.code === "FTW_BOOTSTRAP_RATE_LIMITED" && /429/.test(err.message),
  );
});

// ---- source hygiene: lock in the enroll.html bootstrap-courier contract ----

const __dirname = dirname(fileURLToPath(import.meta.url));
const ENROLL = readFileSync(join(__dirname, "enroll.html"), "utf8");

test("enroll.html reads #b= from the hash and CLEARS it (no lingering secret)", () => {
  assert.match(ENROLL, /bootstrapIdFromHash\(location\.hash\)/);
  assert.match(ENROLL, /history\.replaceState/, "the bootstrap_id must be cleared from history");
});

test("enroll.html claims + VERIFIES before enrolling (verify-before-trust)", () => {
  assert.match(ENROLL, /claimAndVerify\(/, "must claim + verify the Pi descriptor first");
  // The verified entry feeds the directory seed (a keyless device can't hit the
  // P2P-only /instance-descriptor), so the seed uses verifiedEntry.
  assert.match(ENROLL, /verifiedEntry/);
});

test("enroll.html sends claim_key (relay gate) AND pin (Pi factor) on start AND finish", () => {
  assert.match(ENROLL, /claim_key=/);
  assert.match(ENROLL, /claimKeyFromBootstrapId/);
  // pin still rides through to the Pi.
  assert.match(ENROLL, /pin=/);
  // Both enroll RPCs go through the transport selector that adds claim_key/pin.
  assert.match(ENROLL, /enrollFetch\("start"/);
  assert.match(ENROLL, /enrollFetch\("finish"/);
});

test("enroll.html renders support details for failed enroll RPCs", () => {
  assert.match(ENROLL, /X-FTW-Error-Code/);
  assert.match(ENROLL, /Support details/);
  assert.match(ENROLL, /Setup handle/);
  assert.match(ENROLL, /FTW_REMOTE_P2P_ONLY/);
});

test("enroll.html bootstrap path raw-fetches the relay (NOT ownerFetch/P2P) — keyless device", () => {
  // The selector falls back to a raw fetch when a bootstrap_id is present.
  assert.match(ENROLL, /if \(bootstrapId\) \{\s*return fetch\(/s);
});

test("enroll.html sends a ceremony-bound, BODY-bound bootstrap_proof on FINISH (closes the relay-sees-PIN + device_pubkey-swap HIGHs)", () => {
  // The finish query must carry bootstrap_proof, computed via the exported helper
  // over (bootstrap_id, ceremony_token, bodyString) — NOT over claim_key or pin.
  assert.match(ENROLL, /bootstrapProof\(/, "must compute the possession proof with bootstrapProof()");
  assert.match(ENROLL, /bootstrap_proof=/, "finish must send &bootstrap_proof=");
  // The proof now binds the ceremony_token AND the exact finish body string.
  assert.match(ENROLL, /bootstrapProof\(\s*bootstrapId\s*,\s*ceremony_token\s*,\s*bodyString\s*\)/);
});

test("enroll.html serializes the finish body ONCE and POSTs it VERBATIM (the proof hashes the exact bytes sent)", () => {
  // bodyString is JSON.stringify'd once...
  assert.match(ENROLL, /const\s+bodyString\s*=\s*JSON\.stringify\(\s*finishBody\s*\)/);
  // ...and the SAME string is the request body (not a re-stringify of a different
  // object), so the Pi's hash over the received bytes equals the browser's.
  assert.match(ENROLL, /body:\s*bodyString\b/, "the finish POST body must be the verbatim bodyString");
  // device_pubkey is attached to finishBody BEFORE it is serialized + hashed, so the
  // proof binds the C4 key a MITM relay would otherwise swap.
  assert.match(ENROLL, /finishBody\.device_pubkey\s*=\s*devicePubHex/);
});

test("enroll.html keeps bootstrap_id for the whole ceremony but the raw id NEVER leaves the browser", () => {
  // The URL hash is still cleared immediately on read (no regression).
  assert.match(ENROLL, /history\.replaceState/);
  // The raw bootstrap_id must NOT be appended to any request query. Only its
  // sha256 digest (claim_key) and its HMAC (bootstrap_proof) leave the browser.
  assert.doesNotMatch(ENROLL, /bootstrap_id=/, "the raw bootstrap_id must never be sent on the wire");
  // The finish-query builder must include the proof alongside the existing factors.
  assert.match(ENROLL, /encodeURIComponent\(proof\)/, "the proof is url-encoded into the finish query");
});
