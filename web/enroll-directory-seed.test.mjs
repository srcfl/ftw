// node --test web/enroll-directory-seed.test.mjs
//
// First-enrollment directory seeding (Task Group 6, requirement 5) and the
// gate-state routing DECISION given a mock directory (0 / 1 / >1 instances).
//
// Two layers:
//  1. Behavioural — the enroll flow builds a 1-entry directory from the Pi-signed
//     instance descriptor and writes it via instance-sync.saveDirectory. We
//     exercise the REAL instance-sync.js against a fake relay + the Go-signed
//     fixture, proving the entry shape the enroll page builds round-trips
//     (saveDirectory → getCachedInstances → loadDirectory) and verifies.
//  2. Pure decision — the route() contract from next-app.js
//     openDirectoryAfterAssertion: 0 → guidance, 1 → auto-open, >1 → auto-open
//     the first (picker TODO). Replicated here as a pure function so the
//     0/1/>1 branch is asserted headless (next-app.js itself is a DOM-coupled
//     IIFE the repo can't import — see web/setup.test.mjs).

import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";

// ---- shared web-crypto + storage harness so instance-sync.js runs under node --
import { webcrypto } from "node:crypto";
if (!globalThis.crypto) globalThis.crypto = webcrypto;
globalThis.btoa = (s) => Buffer.from(s, "binary").toString("base64");
globalThis.atob = (s) => Buffer.from(s, "base64").toString("binary");

// Minimal localStorage so getCachedInstances/cacheInstances work.
function installLocalStorage() {
  const store = new Map();
  globalThis.localStorage = {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  };
  return store;
}

// Fake relay: a single in-memory wallet blob behind /wallet/<W>/blob.
function installFakeRelay() {
  const blobs = new Map(); // W -> { ciphertext, nonce, version, write_pub }
  globalThis.fetch = async (url, init = {}) => {
    const m = /\/wallet\/([^/]+)\/blob$/.exec(String(url));
    if (!m) throw new Error("unexpected fetch " + url);
    const W = m[1];
    if (!init.method || init.method === "GET") {
      const b = blobs.get(W);
      if (!b) return { status: 404, ok: false, json: async () => null };
      return { status: 200, ok: true, json: async () => b };
    }
    if (init.method === "PUT") {
      const body = JSON.parse(init.body);
      blobs.set(W, { ciphertext: body.ciphertext, nonce: body.nonce, version: body.version, write_pub: body.write_pub });
      return { status: 200, ok: true };
    }
    throw new Error("unexpected method " + init.method);
  };
  return blobs;
}

// The Go(Pi)-signed instance descriptor fixture (same as instance-interop.test.mjs):
// exactly what GET /api/owner-access/instance-descriptor returns and what the
// enroll page wraps into the 1-entry directory.
const DESC = {
  site_id: "site:Home",
  label: "Home",
  pi_pubkey:
    "f0298dc888bb1d4c2d5708ae5072d6c4ae282d00fd705c42534ab2864bdc6e81e914516a4452b431ae6eb138afaeb8ad68300c61829530a6106622cb6c8858d2",
  sig: "8nDdyLDhqHIug9__TseI7bUeFI8IsfyWKPi_ND62-iMAKqmZnmEtOet_QbEF3G1DKj1E7b2ngNpUpXwvHnCA5Q",
};

describe("first-enrollment directory seed (instance-sync round-trip)", () => {
  let store, blobs, sync, prf;
  beforeEach(async () => {
    store = installLocalStorage();
    blobs = installFakeRelay();
    sync = await import("./owner-access/instance-sync.js?seed=" + Math.random());
    prf = await import("./owner-access/prf.js?seed=" + Math.random());
  });

  it("the descriptor verifies as an instance entry (verifyEntry over the Pi sig)", async () => {
    assert.ok(await sync.verifyEntry(DESC), "Pi-signed descriptor must verify");
  });

  it("saveDirectory writes a 1-entry directory; getCachedInstances reads it back", async () => {
    const W = "dGVzdC13YWxsZXQ"; // base64url(user.id), opaque
    const encKey = await prf.deriveEncKey(new Uint8Array(32).fill(7).buffer);
    const entry = { ...DESC, added_ms: Date.now() };
    const out = await sync.saveDirectory(W, encKey, "https://relay.example", {
      instances: [entry], version: 0, writeKey: null,
    });
    assert.equal(out.putStatus, 200, "relay PUT must succeed");
    const cached = sync.getCachedInstances();
    assert.equal(cached.length, 1, "browser-carried copy holds the single home");
    assert.equal(cached[0].site_id, "site:Home");
    assert.equal(cached[0].pi_pubkey, DESC.pi_pubkey);
    assert.ok(blobs.has(W), "an encrypted blob was written to the relay");
  });

  it("a FRESH device with the same passkey (encKey) loads the seeded home from the relay", async () => {
    const W = "dGVzdC13YWxsZXQ";
    const encKey = await prf.deriveEncKey(new Uint8Array(32).fill(7).buffer);
    await sync.saveDirectory(W, encKey, "https://relay.example", {
      instances: [{ ...DESC, added_ms: Date.now() }], version: 0, writeKey: null,
    });
    // Simulate a fresh device: wipe the browser-carried copy, keep the relay blob.
    store.delete("ftw.directory");
    assert.equal(sync.getCachedInstances().length, 0, "fresh device starts with no cache");
    const dir = await sync.loadDirectory(W, encKey, "https://relay.example");
    assert.ok(dir, "directory loads from the relay blob");
    assert.equal(dir.instances.length, 1, "the seeded home is discovered remotely");
    assert.equal(dir.instances[0].site_id, "site:Home");
  });

  it("without PRF (encKey=null) saveDirectory still caches locally but writes NO relay blob", async () => {
    const W = "dGVzdC13YWxsZXQ";
    const entry = { ...DESC, added_ms: Date.now() };
    const out = await sync.saveDirectory(W, null, "https://relay.example", {
      instances: [entry], version: 0, writeKey: null,
    });
    assert.equal(sync.getCachedInstances().length, 1, "browser-carried copy still works with no PRF");
    assert.equal(blobs.has(W), false, "no encrypted blob without a key (carry-local only)");
    assert.equal(out.putStatus, undefined, "no relay PUT attempted without encKey");
  });
});

// route() is the exact decision the next-app.js openDirectoryAfterAssertion makes
// after loadDirectory resolves. Replicated as a pure function so the 0/1/>1
// branch is covered headless. Returns the routing verdict as a tag.
function route(instances) {
  const list = instances || [];
  if (list.length === 1) return "auto-open";
  if (list.length > 1) return "auto-open-first"; // picker TODO
  return "needs-setup-guidance"; // 0 entries — not an error
}

describe("gate-state decision: 0 / 1 / >1 instances", () => {
  it("0 instances → finish-setup guidance (no error, no open)", () => {
    assert.equal(route([]), "needs-setup-guidance");
  });
  it("exactly 1 instance → auto-open", () => {
    assert.equal(route([{ site_id: "site:Home" }]), "auto-open");
  });
  it(">1 instances → auto-open the first (picker deferred)", () => {
    assert.equal(route([{ site_id: "site:Home" }, { site_id: "site:Cabin" }]), "auto-open-first");
  });
});

// ---- static wiring guards: enroll.html + login.html consume the real contract --
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");

describe("enroll.html seeds the directory on first enrollment", () => {
  const ENROLL = read("owner-access/enroll.html");
  it("requests the PRF extension on navigator.credentials.create", () => {
    assert.match(ENROLL, /extensionInput\(\)/);
    assert.match(ENROLL, /credOpts\.extensions\s*=/);
  });
  it("fetches the Pi-signed instance descriptor over the owner channel", () => {
    assert.match(ENROLL, /ownerFetch\("\/api\/owner-access\/instance-descriptor"/);
  });
  it("verifies the entry, then saveDirectory with a 1-entry list (version 0, writeKey null)", () => {
    assert.match(ENROLL, /verifyEntry\(entry\)/);
    assert.match(ENROLL, /saveDirectory\(/);
    assert.match(ENROLL, /instances:\s*\[entry\],\s*version:\s*0,\s*writeKey:\s*null/);
  });
  it("derives the key from the create assertion's PRF output (null fallback)", () => {
    assert.match(ENROLL, /outputFrom\(cred\)/);
    assert.match(ENROLL, /prfOut\s*\?\s*await\s+deriveEncKey\(prfOut\)\s*:\s*null/);
  });
});

describe("login.html requests PRF and seeds the routing directory", () => {
  const LOGIN = read("owner-access/login.html");
  it("requests the PRF extension on the assertion", () => {
    assert.match(LOGIN, /extensionInput\(\)/);
    assert.match(LOGIN, /credOpts\.extensions\s*=/);
  });
  it("loads the directory from THIS assertion's PRF output after sign-in", () => {
    assert.match(LOGIN, /outputFrom\(cred\)/);
    assert.match(LOGIN, /loadDirectory\(/);
  });
});
