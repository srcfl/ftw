// node --test web/home-route-silent-auth.test.mjs
//
// Wiring + contract guards for the silent device-key home route (Phase 5):
//   C2 — p2p.js proves the device to the RELAY before posting a WebRTC offer.
//   C3 — next-app.js mints the owner session SILENTLY over the open channel
//        (device-PoP), trying it BEFORE the passkey ceremony.
//   C4 — enroll.html sends the device pubkey in enroll/finish so the Pi pins it.
// These are static guards (the real crypto is covered in device-key.test.mjs);
// they lock in the exact signing strings + field names so the relay/Pi agents
// stay byte-for-byte aligned, and the silent-first ordering Fredrik asked for.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");

const P2P = read("p2p.js");
const APP = read("next-app.js");
const ENROLL = read("owner-access/enroll.html");
const INDEX = read("index.html");

describe("C2 — p2p.js proves the device to the relay before the offer", () => {
  it("GETs the per-site challenge endpoint", () => {
    assert.match(P2P, /\/signal\/"\s*\+\s*encodeURIComponent\(site\)\s*\+\s*"\/challenge/,
      "must GET /signal/<site>/challenge");
  });

  it("signs the exact C2 string ftw-signal:v1:<site>:<nonce>", () => {
    assert.match(P2P, /"ftw-signal:v1:"\s*\+\s*site\s*\+\s*":"\s*\+\s*ch\.nonce/,
      "signing string must be ftw-signal:v1:<site>:<nonce> byte-for-byte");
  });

  it("attaches {device_pubkey, nonce, sig} to the offer POST body", () => {
    // The offer body is now a JSON envelope carrying the SDP + the proof fields.
    assert.match(P2P, /sdp:\s*pc\.localDescription\.sdp/, "offer JSON carries the raw SDP");
    assert.match(P2P, /device_pubkey:\s*proof\.device_pubkey/);
    assert.match(P2P, /nonce:\s*proof\.nonce/);
    assert.match(P2P, /sig:\s*proof\.sig/);
    assert.match(P2P, /headers:\s*\{\s*"Content-Type":\s*"application\/json"\s*\}/,
      "offer POST is application/json (envelope), not raw application/sdp");
  });

  it("fails closed (no offer) when this device has no key — sets the unenrolled state", () => {
    assert.match(P2P, /hasDeviceKey\(\)/, "must check for a device key (without minting)");
    assert.match(P2P, /code\s*=\s*"no-device-key"/);
    assert.match(P2P, /unenrolled\s*=\s*true/);
    assert.match(P2P, /isUnenrolled:/, "exposes isUnenrolled() for the gate");
  });

  it("exposes site() so next-app.js can build the C3 signing string off the SAME site", () => {
    assert.match(P2P, /site:\s*function\s*\(\)\s*\{\s*return\s*pinnedIdentity\(\)/);
  });
});

describe("C3 — next-app.js mints the session silently (device-PoP), passkey only as fallback", () => {
  it("GETs the device-challenge and POSTs device-pop over ownerFetch (strict, never bare)", () => {
    assert.match(APP, /ownerFetch\("\/api\/owner-access\/device-challenge"/,
      "device-challenge must ride ownerFetch (strict), not bare fetch");
    assert.match(APP, /ownerFetch\("\/api\/owner-access\/device-pop"/,
      "device-pop must ride ownerFetch (strict), not bare fetch");
  });

  it("signs the exact C3 string ftw-device-pop:v1:<site>:<challenge>", () => {
    assert.match(APP, /"ftw-device-pop:v1:"\s*\+\s*site\s*\+\s*":"\s*\+\s*ch\.challenge/,
      "signing string must be ftw-device-pop:v1:<site>:<challenge> byte-for-byte");
  });

  it("POSTs {device_pubkey, challenge, sig} as the device-pop body", () => {
    assert.match(APP, /device_pubkey:\s*key\.pubHex/);
    assert.match(APP, /challenge:\s*ch\.challenge/);
    assert.match(APP, /sig:\s*sig/);
  });

  it("tries the SILENT path FIRST and falls back to the passkey ceremony", () => {
    // runSignIn calls runDevicePoP() before runPasskeySignIn().
    assert.match(APP, /runDevicePoP\(\)\.then\(function\s*\(silentOk\)/);
    assert.match(APP, /return runPasskeySignIn\(say\)/,
      "passkey ceremony is the fallback, not the first move");
    // setupAuth attempts silent auth before showing the gate.
    assert.match(APP, /runDevicePoP\(\)\.then\(function\s*\(ok\)\s*\{[\s\S]*?showSignInGate\(\)/,
      "setupAuth must try silent device-PoP before showing the sign-in gate");
  });

  it("does not mint a key just to probe (hasDeviceKey gate before getOrCreate)", () => {
    assert.match(APP, /window\.ftwDeviceKey\.hasDeviceKey\(\)/);
  });
});

describe("C4 — enroll.html pins the device key at LAN enrollment", () => {
  it("imports the device-key store and sends device_pubkey in enroll/finish", () => {
    assert.match(ENROLL, /import\s*\{\s*exportPubHex\s*\}\s*from\s*["']\.\/device-key\.js["']/);
    assert.match(ENROLL, /finishBody\.device_pubkey\s*=\s*devicePubHex/,
      "enroll/finish body must carry device_pubkey (128hex) for the Pi to pin");
  });

  it("merges device_pubkey INTO the WebAuthn registration JSON (one body, C4) and POSTs it verbatim", () => {
    assert.match(ENROLL, /const\s+finishBody\s*=\s*encodeRegistrationResult\(cred\)/);
    // The body is serialized ONCE (so the body-bound bootstrap_proof hashes the exact
    // bytes sent) and POSTed verbatim as bodyString — not re-stringified inline.
    assert.match(ENROLL, /const\s+bodyString\s*=\s*JSON\.stringify\(\s*finishBody\s*\)/);
    assert.match(ENROLL, /body:\s*bodyString\b/);
  });
});

describe("index.html loads the device-key module before p2p.js consumes it", () => {
  it("includes the device-key.js module script", () => {
    assert.match(INDEX, /<script[^>]*type="module"[^>]*src="\/owner-access\/device-key\.js/,
      "device-key.js must be loaded so window.ftwDeviceKey exists for p2p.js/next-app.js");
  });
});

describe("UI clarity — the gate makes the security legible (Fredrik's ask)", () => {
  it("the gate carries a trust line conveying direct + E2E + relay-blind", () => {
    assert.match(INDEX, /signin-gate-trust/);
    assert.match(INDEX, /id="signin-gate-trust-text"/);
    // next-app.js drives its copy from the live transport state.
    assert.match(APP, /signin-gate-trust-text/);
    assert.match(APP, /relay never sees your home|never sees your home|forwards ciphertext/i);
  });

  it("conveys 'this device is remembered' on a silent sign-in", () => {
    assert.match(APP, /this device is remembered/i);
  });

  it("offers a clear 'set up this device first' state instead of an unsatisfiable passkey prompt", () => {
    assert.match(APP, /showGate\(unEnrolled\s*\?\s*"setup"\s*:\s*"signin"\)/);
    assert.match(INDEX, /signin-gate-needs-setup/);
    assert.match(INDEX, /Set up this device/i);
  });
});
