// node --test web/landing-gate.test.mjs
//
// Gate routing (Task Group 6): next-app.js must show the PUBLIC landing when
// there's no decryptable directory (anonymous), wire the landing button to the
// same runSignIn ceremony, AUTO-OPEN when the decrypted directory has exactly 1
// entry (no picker in v1), and request the PRF extension on the assertion. These
// static guards lock the exact branch shape against the prf.js / instance-sync.js
// contract (window.ftwPrf, window.ftwInstanceSync) so the modules stay aligned.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");
const APP = read("next-app.js");
const INDEX = read("index.html");

describe("next-app.js — public landing for the anonymous visitor", () => {
  it("shows the public-landing gate mode when no directory is decryptable", () => {
    assert.match(APP, /showGate\("public-landing"\)/,
      "an anonymous / no-directory visitor must land on data-mode=public-landing");
  });

  it("reads the cached directory via instance-sync (getCachedInstances)", () => {
    assert.match(APP, /getCachedInstances\(\)/,
      "the gate must consult the decrypted directory, not invent its own store");
  });

  it("wires the landing button to the SAME runSignIn ceremony as the gate button", () => {
    assert.match(APP, /getElementById\("signin-landing-btn"\)/);
    assert.match(APP, /landingBtn[\s\S]{0,180}runSignIn\(\{\s*allowSilent:\s*false\s*\}\)/,
      "the landing button must call the shared runSignIn ceremony, but never silently after an explicit click");
  });
});

describe("next-app.js — PRF extension on the sign-in assertion (prf.js contract)", () => {
  it("requests the prf extension via extensionInput() on navigator.credentials.get", () => {
    assert.match(APP, /extensionInput\(\)/,
      "the assertion must carry prf.extensionInput() so PRF is evaluated");
    assert.match(APP, /publicKey\.extensions\s*=/,
      "navigator.credentials.get publicKey must set an extensions field for PRF");
  });

  it("derives the directory key from the assertion via outputFrom + deriveEncKey", () => {
    assert.match(APP, /outputFrom\(/,
      "must read the PRF output from the assertion (prf.outputFrom)");
    assert.match(APP, /deriveEncKey\(/,
      "must HKDF the PRF output into the AES key (prf.deriveEncKey)");
  });

  it("uses the base64url userHandle as the wallet handle W", () => {
    assert.match(APP, /userHandle/,
      "W = base64url(assertion.response.userHandle) keys the relay blob");
  });

  it("loads the directory via instance-sync.loadDirectory(W, encKey, origin)", () => {
    assert.match(APP, /loadDirectory\(/,
      "must call instanceSync.loadDirectory to fetch+decrypt the relay blob");
    assert.match(APP, /location\.origin/,
      "loadDirectory's relayBase is location.origin on the public home route");
  });

  it("falls back to loadDirectory(W, null, origin) when PRF is unavailable", () => {
    assert.match(APP, /loadDirectory\([^)]*,\s*null\s*,/,
      "no PRF → browser-carried copy via loadDirectory(W, null, origin)");
  });
});

describe("next-app.js — auto-open on exactly 1 entry (no picker in v1)", () => {
  it("auto-opens when the directory has exactly one entry", () => {
    assert.match(APP, /length\s*===\s*1/,
      "exactly-1-entry must short-circuit straight through (no picker)");
  });

  it("auto-opens the FIRST entry when there is more than one (picker TODO)", () => {
    assert.match(APP, /TODO[\s\S]{0,120}picker/i,
      "the >1 case auto-opens the first and leaves a clearly-marked picker TODO");
  });

  it("does NOT render a picker UI in v1", () => {
    assert.doesNotMatch(APP, /pickInstance|instance-picker|chooseInstance/i,
      "v1 routes the single home automatically; the picker is deferred");
  });
});

describe("index.html loads the multi-tenant crypto modules (prf + instance-sync)", () => {
  it("loads prf.js and instance-sync.js so window.ftwPrf / window.ftwInstanceSync exist", () => {
    assert.match(INDEX, /owner-access\/prf\.js/,
      "prf.js must be loaded for the PRF-derived directory key");
    assert.match(INDEX, /owner-access\/instance-sync\.js/,
      "instance-sync.js must be loaded for loadDirectory/saveDirectory/getCachedInstances");
  });
});
