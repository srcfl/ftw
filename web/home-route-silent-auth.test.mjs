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

  it("attaches {device_pubkey, nonce, sig} to the offer POST body when a proof exists", () => {
    // With a device-key proof, the offer body is a JSON envelope carrying the SDP
    // + proof fields. This keeps -require-device-key compatible when explicitly on.
    assert.match(P2P, /sdp:\s*pc\.localDescription\.sdp/, "offer JSON carries the raw SDP");
    assert.match(P2P, /device_pubkey:\s*proof\.device_pubkey/);
    assert.match(P2P, /nonce:\s*proof\.nonce/);
    assert.match(P2P, /sig:\s*proof\.sig/);
    assert.match(P2P, /headers:\s*\{\s*"Content-Type":\s*"application\/json"\s*\}/,
      "offer POST is application/json (envelope), not raw application/sdp");
  });

  it("posts a proofless raw-SDP offer when this device has no key", () => {
    assert.match(P2P, /hasDeviceKey\(\)/, "must check for a device key (without minting)");
    assert.match(P2P, /code\s*=\s*"no-device-key"/);
    assert.match(P2P, /e\.code\s*===\s*"no-device-key"[\s\S]*e\.code\s*===\s*"store-pending"[\s\S]*return null;/);
    assert.match(P2P, /"Content-Type":\s*"application\/sdp"/,
      "missing device key should send raw SDP for relays with the gate off");
    assert.doesNotMatch(P2P, /code\s*===\s*"no-device-key"[\s\S]{0,80}unenrolled\s*=\s*true/,
      "missing device key must not abort the offer path");
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

  it("tries silent auth automatically before showing the gate", () => {
    assert.match(APP, /runDevicePoP\(\)\.then\(function\s*\(ok\)\s*\{[\s\S]*?showSignInGate\(\)/,
      "setupAuth must try silent device-PoP before showing the sign-in gate");
  });

  it("the explicit passkey buttons do not silently log in a remembered browser", () => {
    assert.match(APP, /gateBtn\.onclick\s*=\s*function\s*\(\)\s*\{\s*runSignIn\(\{\s*allowSilent:\s*false\s*\}\)/,
      "the returning-visitor button says passkey, so it must not run C3 first");
    assert.match(APP, /landingBtn\.onclick\s*=\s*function\s*\(\)\s*\{\s*runSignIn\(\{\s*allowSilent:\s*false\s*\}\)/,
      "the public landing button says passkey, so it must not run C3 first");
    assert.match(APP, /var allowSilent\s*=\s*opts\.allowSilent\s*===\s*true\s*&&\s*!manualSignoutActive\(\)/);
    assert.match(APP, /if \(!allowSilent\) return runPasskeySignIn\(say\)/);
  });

  it("manual logout suppresses silent device auth until a passkey succeeds", () => {
    assert.match(APP, /MANUAL_SIGNOUT_KEY\s*=\s*"ftw\.owner\.manual_signout\.v1"/);
    assert.match(APP, /function markManualSignout\(\)[\s\S]*localStorage\.setItem\(MANUAL_SIGNOUT_KEY,\s*"1"\)/);
    assert.match(APP, /signoutBtn\.onclick[\s\S]*markManualSignout\(\)[\s\S]*ownerFetch\("\/api\/owner-access\/logout"/);
    assert.match(APP, /if \(manualSignoutActive\(\)\)\s*\{\s*showSignInGate\(\);[\s\S]*?return;\s*\}[\s\S]*?if \(!silentAuthTried\)/,
      "setupAuth must not run C3 while the user is explicitly signed out");
    assert.match(APP, /clearManualSignout\(\);[\s\S]*say\("Signed in\.",\s*"ok"\)/,
      "a successful passkey ceremony re-enables remembered-device login for later reloads");
  });

  it("waits briefly for the device-key module before burning the silent-auth attempt", () => {
    assert.match(APP, /function waitForDeviceKeyStore\(timeoutMs\)/,
      "module-script device-key.js can load after next-app.js; silent auth must wait for it");
    assert.match(APP, /return waitForDeviceKeyStore\(3000\)/,
      "runDevicePoP must wait for window.ftwDeviceKey before falling back to passkey");
  });

  it("does not burn the one silent-auth attempt before P2P is direct", () => {
    assert.match(APP, /function ownerTransportReady\(\)/,
      "setupAuth needs a cheap readiness check before consuming silentAuthTried");
    assert.match(APP, /if \(!ownerTransportReady\(\)\)\s*\{\s*showWaitingOrLandingGate\(\);[\s\S]*?return;\s*\}[\s\S]*?silentAuthTried\s*=\s*true/,
      "setupAuth must stay in connecting until direct transport exists, then try silent auth");
    assert.match(APP, /function showWaitingOrLandingGate\(\)[\s\S]*?!hasDecryptableDirectory\(\)[\s\S]*?showSignInGate\(\)/,
      "fresh public browsers without a directory must land on setup/sign-in instead of guessing a site_id");
    assert.match(APP, /function scheduleAuthRetry\(delayMs\)/,
      "connecting auth checks must retry so Chrome cannot stay on the initial gate forever");
    assert.match(APP, /return waitForOwnerTransport\(10000\)[\s\S]*?return waitForDeviceKeyStore\(3000\)/,
      "runDevicePoP itself must wait for direct owner transport before challenge/pop");
  });

  it("suppresses setup/no-devices prompts while the auth gate is connecting", () => {
    assert.match(APP, /var authGateActive\s*=\s*false/,
      "the dashboard needs a separate auth-pending state, not just ownerNotAuthed");
    assert.match(APP, /if \(authGateActive \|\| ownerNotAuthed\) return;/,
      "setup banner must not flash while remote auth is still connecting");
    assert.match(APP, /if \(authGateActive \|\| ownerNotAuthed\)\s*\{ if \(existing\) existing\.remove\(\); return; \}/,
      "no-devices prompt must be removed while auth is pending or logged out");
    assert.match(APP, /authGateActive\s*=\s*true;[\s\S]*?hideSetupBanner\(\)/,
      "showGate must actively clear stale setup chrome");
  });

  it("does not poll owner data behind the remote auth gate", () => {
    assert.match(APP, /function ownerDataAllowed\(\)[\s\S]*!authGateActive\s*&&\s*!ownerNotAuthed/,
      "owner data fetches should be paused until remote auth is resolved");
    assert.match(APP, /function fetchStatus\(\)\s*\{\s*if \(!ownerDataAllowed\(\)\) return Promise\.resolve\(false\);/,
      "hot status poll must not run behind the gate");
    assert.match(APP, /function loadHistory\(range\)\s*\{\s*if \(!ownerDataAllowed\(\)\) return Promise\.resolve\(null\);/,
      "history load must not run behind the gate");
    assert.match(APP, /function fetchLiveHistory\(force\)\s*\{\s*if \(!ownerDataAllowed\(\)\) return Promise\.resolve\(\);/,
      "live history fetch must not run behind the gate");
    assert.match(APP, /function primeOwnerData\(\)[\s\S]*refreshOwnerData\(true\)/,
      "signed-in reloads should prime data exactly after auth");
    assert.match(APP, /me && me\.can_sign_out[\s\S]*hideGate\(\);[\s\S]*primeOwnerData\(\)/,
      "remote signed-in sessions must load data after the gate drops");
  });

  it("does not mint a key just to probe (hasDeviceKey gate before getOrCreate)", () => {
    assert.match(APP, /store\.hasDeviceKey\(\)/);
  });

  it("attaches this browser's device_pubkey on passkey login so reload can silent-login later", () => {
    assert.match(APP, /window\.ftwDeviceKey\.exportPubHex\(\)/,
      "passkey login must mint/read the browser device key");
    assert.match(APP, /finishBody\.device_pubkey\s*=\s*devicePubHex/,
      "login/finish must attach device_pubkey for the Pi's device-key upgrade path");
    assert.match(APP, /body:\s*JSON\.stringify\(finishBody\)/,
      "login/finish must POST the augmented finish body, not a fresh assertion-only object");
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

  it("keeps the passkey wait state neutral while the P2P channel is still opening", () => {
    assert.match(APP, /waitForOwnerTransport\(25000\)/,
      "explicit passkey login should allow slow Safari/Chrome WebRTC setup");
    assert.match(APP, /Still opening the encrypted channel to your Pi\. Try again in a moment\./);
    assert.doesNotMatch(APP, /Secure channel unavailable — retry in a moment\./,
      "the first visible feedback should not look like a hard failure");
  });
});
