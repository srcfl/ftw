// node --test web/owner-access/owner-fetch.test.mjs
//
// FIX-B guards for the owner/CONTROL fetch path. ownerFetch must:
//   - prefer window.p2pFetchStrict (strict transport) when present;
//   - FAIL CLOSED on a PUBLIC origin when no transport loaded (p2p.js never
//     loaded) — the owner body must NOT be raw-fetched to the relay;
//   - raw-fetch only on a genuine-LAN origin.
// Plus source-hygiene over the ceremony pages + dashboard so no owner/control
// call regresses back to a bare fetch().

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));

// --- minimal browser-global harness ---------------------------------------
function setOrigin(pathname, hostname) {
  globalThis.location = { pathname, hostname };
}
let lastRawFetch = null;
beforeEach(() => {
  lastRawFetch = null;
  globalThis.window = {};
  globalThis.Headers = class { };
  globalThis.fetch = (url, opts) => {
    lastRawFetch = { url, opts };
    return Promise.resolve({ ok: true, status: 200, _raw: true });
  };
});
afterEach(() => {
  delete globalThis.window;
  delete globalThis.location;
  delete globalThis.fetch;
  delete globalThis.Headers;
});

const { ownerFetch, isLanOrigin } = await import("./owner-fetch.js");

describe("ownerFetch — transport selection (FIX-B)", () => {
  it("uses window.p2pFetchStrict when present (strict transport)", async () => {
    setOrigin("/", "home.fortytwowatts.com");
    let calledStrict = null;
    globalThis.window.p2pFetchStrict = (path, opts) => {
      calledStrict = { path, opts };
      return Promise.resolve({ ok: true, status: 200, _strict: true });
    };
    const r = await ownerFetch("/api/owner-access/login/start", { method: "POST" });
    assert.equal(r._strict, true, "must route through the strict transport");
    assert.equal(calledStrict.path, "/api/owner-access/login/start");
    assert.equal(lastRawFetch, null, "must NOT raw-fetch when strict is available");
  });

  it("FAILS CLOSED on a PUBLIC origin when no transport loaded", async () => {
    setOrigin("/", "home.fortytwowatts.com"); // dotted public host, relay not strictly in path but NOT LAN
    // no window.p2pFetchStrict, no window.ftwP2P → transport absent
    const r = await ownerFetch("/api/owner-access/login/start", { method: "POST" });
    assert.equal(r.ok, false);
    assert.equal(r.status, 503, "public-origin no-transport must fail closed with 503");
    assert.equal(lastRawFetch, null, "owner body must NEVER be raw-fetched to the relay on a public origin");
  });

  it("FAILS CLOSED under a /me/<site> relay tunnel prefix with no transport", async () => {
    setOrigin("/me/site:Home/owner-access/login.html", "relay.fortytwowatts.com");
    const r = await ownerFetch("/api/owner-access/login/finish", { method: "POST" });
    assert.equal(r.status, 503, "relay-prefixed origin is not LAN → fail closed");
    assert.equal(lastRawFetch, null);
  });

  it("raw-fetches on a genuine LAN origin when no transport loaded", async () => {
    setOrigin("/owner-access/login.html", "192.168.1.42");
    const r = await ownerFetch("/api/owner-access/login/start", { method: "POST" });
    assert.equal(r._raw, true, "LAN origin may raw-fetch");
    assert.ok(lastRawFetch, "raw fetch should have been issued on LAN");
  });

  it("treats *.local and single-label hosts as LAN", async () => {
    setOrigin("/owner-access/login.html", "fortytwowatts.local");
    assert.equal(isLanOrigin(), true);
    setOrigin("/owner-access/login.html", "raspberrypi");
    assert.equal(isLanOrigin(), true);
  });

  it("treats a dotted public host as NOT LAN (fail closed)", async () => {
    setOrigin("/", "home.fortytwowatts.com");
    assert.equal(isLanOrigin(), false);
  });

  it("prefers p2p.js's own isLanOrigin when available (single source of truth)", async () => {
    setOrigin("/", "home.fortytwowatts.com");
    globalThis.window.ftwP2P = { isLanOrigin: () => true }; // p2p.js says LAN
    assert.equal(isLanOrigin(), true, "must defer to p2p.js's isLanOrigin");
  });
});

// --- source hygiene: no owner/control call may regress to a bare fetch ------
describe("ceremony pages route owner calls through the shared strict ownerFetch", () => {
  const LOGIN = readFileSync(join(__dirname, "login.html"), "utf8");
  const ENROLL = readFileSync(join(__dirname, "enroll.html"), "utf8");
  const INDEX = readFileSync(join(__dirname, "index.html"), "utf8");

  for (const [name, html] of [["login.html", LOGIN], ["enroll.html", ENROLL], ["index.html", INDEX]]) {
    it(`${name} imports the shared ownerFetch and has no inline raw-fetch fallback`, () => {
      assert.match(html, /import\s*\{[^}]*\bownerFetch\b[^}]*\}\s*from\s*["']\.\/owner-fetch\.js["']/,
        "must import the shared ownerFetch");
      // The old leaky fallback was: return fetch(base + path, ...). It must be gone.
      assert.doesNotMatch(html, /return\s+fetch\(\s*base\s*\+\s*path/,
        "must NOT raw-fetch the owner body to the relay when p2p.js is absent");
    });
  }

  it("public owner-access sign-in converges on the dashboard gate, not legacy login.html", () => {
    assert.match(INDEX, /id="signin-link"/, "landing link must be addressable");
    assert.match(INDEX, /const\s+onLan\s*=\s*isLanOrigin\(\)/,
      "public-vs-LAN must use origin detection, not apiBase()==='' which is true on home.*");
    assert.match(INDEX, /signinLink\.href\s*=\s*"\/"/,
      "public owner-access sign-in should route to the root gate");
    assert.match(LOGIN, /if\s*\(!isLanOrigin\(\)\)\s*\{\s*location\.replace\("\/"\)/,
      "direct public hits to legacy login.html should redirect to the root gate");
  });

  it("owner-access signout sets the same local manual-signout guard as the dashboard", () => {
    assert.match(INDEX, /MANUAL_SIGNOUT_KEY\s*=\s*"ftw\.owner\.manual_signout\.v1"/);
    assert.match(INDEX, /function markManualSignout\(\)[\s\S]*localStorage\.setItem\(MANUAL_SIGNOUT_KEY,\s*"1"\)/);
    assert.match(INDEX, /document\.getElementById\("signout"\)\.onclick[\s\S]*markManualSignout\(\)[\s\S]*ownerFetch\("\/api\/owner-access\/logout"/);
  });
});

describe("dashboard control + owner writes route through strict (FIX-B)", () => {
  const APP = readFileSync(join(__dirname, "..", "next-app.js"), "utf8");

  it("defines a single ownerFetch entry point that fails closed off-LAN", () => {
    assert.match(APP, /function ownerFetch\(/);
    assert.match(APP, /ownerWriteFailClosed/);
    assert.match(APP, /p2pFetchStrict/);
  });

  it("routes the state-changing control calls through ownerFetch, not bare fetch", () => {
    // setMode, postJson, driverLifecycleCall, loadpoint /target, sign-out.
    assert.match(APP, /ownerFetch\("\/api\/mode"/);
    assert.match(APP, /return ownerFetch\(url, \{/);             // postJson
    assert.match(APP, /ownerFetch\("\/api\/drivers\/"/);         // driverLifecycleCall
    assert.match(APP, /ownerFetch\("\/api\/loadpoints\/"/);      // loadpoint target
    assert.match(APP, /ownerFetch\("\/api\/owner-access\/logout"/);
    assert.match(APP, /ownerFetch\("\/api\/owner-access\/whoami"/);
  });

  it("no longer bare-fetches /api/mode or the loadpoint /target writes", () => {
    assert.doesNotMatch(APP, /[^r]\bfetch\("\/api\/mode"/);
    assert.doesNotMatch(APP, /\bfetch\("\/api\/loadpoints\/" \+ encodeURIComponent/);
  });
});
