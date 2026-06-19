// node --test web/p2p-identity-pin.test.mjs
//
// Multi-tenant pin (Task Group 6): pinnedIdentity()/site() must key the pin per
// (origin, site_id) at localStorage "ftw.identity:<apiBase>:<site_id>", taking the
// site_id + pi_pubkey from the decrypted instance directory (window.ftwInstanceSync)
// with NO relay /api/identity round-trip.

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const __dirname = dirname(fileURLToPath(import.meta.url));
const P2P_SRC = readFileSync(join(__dirname, "p2p.js"), "utf8");

// A 128-hex (X||Y) public key whose value is irrelevant to the pin-keying logic:
// importP256Pub() is stubbed in the sandbox so we never run real WebCrypto here.
const PUB_A = "a".repeat(128);
const PUB_B = "b".repeat(128);
const SITE_A = "site:" + "a".repeat(32);
const SITE_LAN = "site:" + "b".repeat(32);

// loadP2P evaluates p2p.js inside a vm sandbox. fetchCalls records every fetched
// URL so a test can assert the relay /api/identity endpoint was (not) hit.
function loadP2P({
  pathname = "/",
  hostname = "home.fortytwowatts.com",
  seedStore = {},
  instances = [],
} = {}) {
  const store = new Map(Object.entries(seedStore));
  const fetchCalls = [];
  const win = {
    localStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
      removeItem: (k) => store.delete(k),
    },
    ftwInstanceSync: {
      getCachedInstances: () => instances,
    },
  };
  const sandbox = {
    window: win,
    location: { pathname, hostname },
    localStorage: win.localStorage,
    crypto: {
      getRandomValues: (a) => a,
      // subtle.importKey is hit by the real importP256Pub() in p2p.js. We stub
      // it to resolve a fake "key" so the pin logic runs without WebCrypto.
      subtle: { importKey: () => Promise.resolve({ __fakeKey: true }) },
    },
    Headers: class {},
    fetch: (url) => {
      fetchCalls.push(String(url));
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({
            public_key_hex: PUB_B,
            site_id: SITE_LAN,
          }),
      });
    },
    setTimeout: () => 0,
    clearTimeout: () => {},
    console: { warn() {}, log() {} },
    btoa: (s) => Buffer.from(s, "binary").toString("base64"),
    atob: (s) => Buffer.from(s, "base64").toString("binary"),
    TextEncoder,
    TextDecoder,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(P2P_SRC, sandbox, { filename: "p2p.js" });
  return { win, store, fetchCalls };
}

describe("p2p.js per-(origin, site_id) pin from the directory", () => {
  let h;
  beforeEach(() => {
    h = loadP2P({
      instances: [{ site_id: SITE_A, pi_pubkey: PUB_A, label: "Home" }],
    });
  });
  afterEach(() => {
    h = null;
  });

  it("site() resolves the directory entry's site_id with no relay round-trip", async () => {
    const site = await h.win.ftwP2P.site();
    assert.equal(site, SITE_A);
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      0,
      "the directory carries the Pi pubkey + site_id; /api/identity must NOT be fetched",
    );
  });

  it("pins per (origin, site_id) at ftw.identity:<apiBase>:<site_id>", async () => {
    await h.win.ftwP2P.site(); // triggers pinnedIdentity()
    assert.ok(
      h.store.has("ftw.identity::" + SITE_A),
      "pin key must be ftw.identity:<apiBase>:<site_id> (apiBase is '' on the bare home host)",
    );
    const rec = JSON.parse(h.store.get("ftw.identity::" + SITE_A));
    assert.equal(rec.pub, PUB_A);
    assert.equal(rec.site, SITE_A);
  });
});

describe("p2p.js public-route identity bootstrap", () => {
  it("PUBLIC non-home route: fails closed when nothing is pinned — NEVER trusts relay /api/identity", async () => {
    const h = loadP2P({ hostname: "relay.example.com", instances: [] });
    await assert.rejects(() => h.win.ftwP2P.site(), /no pinned home identity/);
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      0,
      "the public route must never fetch (let alone trust) relay /api/identity",
    );
  });

  it("PUBLIC home route does not guess a universal site_id", async () => {
    const h = loadP2P({ hostname: "home.fortytwowatts.com", instances: [] });
    await assert.rejects(() => h.win.ftwP2P.site(), /no pinned home identity/);
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/signal/site%3AHome/identity") !== -1).length,
      0,
      "home.* fresh browsers must not be hard-routed to site:Home",
    );
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      0,
      "home.* must not use anonymous /api/identity",
    );
  });

  it("PUBLIC route ignores the old single-key identity record", async () => {
    const h = loadP2P({
      hostname: "home.fortytwowatts.com",
      seedStore: { "ftw.identity:": JSON.stringify({ pub: PUB_A, site: "site:Home" }) },
      instances: [],
    });
    await assert.rejects(() => h.win.ftwP2P.site(), /no pinned home identity/);
    assert.equal(h.store.has("ftw.identity::site:Home"), false);
  });

  it("LAN origin: /api/identity TOFU is the safe last resort (the Pi serves it directly)", async () => {
    const h = loadP2P({ hostname: "192.168.1.50", instances: [] });
    const site = await h.win.ftwP2P.site();
    assert.equal(site, SITE_LAN, "on the LAN the Pi answers /api/identity directly — TOFU is safe");
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      1,
    );
  });
});

describe("p2p.js first-key-wins pin", () => {
  it("a directory entry with a DIFFERENT key than the existing pin hard-fails (identity change)", async () => {
    const h = loadP2P({
      hostname: "home.fortytwowatts.com",
      seedStore: { ["ftw.identity::" + SITE_A]: JSON.stringify({ pub: PUB_A, site: SITE_A }) },
      instances: [{ site_id: SITE_A, pi_pubkey: PUB_B, label: "Home" }], // different key
    });
    await assert.rejects(() => h.win.ftwP2P.site(), /home identity .* changed/);
    // The known-good pin is NOT overwritten.
    assert.equal(JSON.parse(h.store.get("ftw.identity::" + SITE_A)).pub, PUB_A);
  });

  it("an identical directory key reuses the pin without error", async () => {
    const h = loadP2P({
      hostname: "home.fortytwowatts.com",
      seedStore: { ["ftw.identity::" + SITE_A]: JSON.stringify({ pub: PUB_A, site: SITE_A }) },
      instances: [{ site_id: SITE_A, pi_pubkey: PUB_A, label: "Home" }],
    });
    assert.equal(await h.win.ftwP2P.site(), SITE_A);
  });
});
