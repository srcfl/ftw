// node --test web/p2p-owner-fetch-wiring.test.mjs
//
// Wiring guard (FIX-B): p2p.js must expose window.ownerFetch as the SAME strict,
// fail-closed function it exposes as window.p2pFetchStrict — so the dashboard's
// classic scripts + web components (which call window.ownerFetch) inherit exactly
// the behaviour the owner-access ceremony pages get via owner-fetch.js: strict P2P
// transport, fail-closed 503 on a public / /me/<site> origin with no DataChannel,
// raw relay fetch ONLY on a genuine-LAN origin. ONE behaviour, shared — never a
// second, forkable implementation.

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const __dirname = dirname(fileURLToPath(import.meta.url));
const P2P_SRC = readFileSync(join(__dirname, "p2p.js"), "utf8");

// loadP2P evaluates p2p.js (a classic IIFE) inside a vm context with a minimal
// browser-global harness, then returns the populated window. RTCPeerConnection is
// LEFT UNDEFINED on purpose so supported() is false and connect() never tries to
// open a real channel at load — we only care about the exposed surface + wiring.
function loadP2P({ pathname = "/", hostname = "home.fortytwowatts.com" } = {}) {
  const store = new Map();
  const fetchCalls = [];
  const win = {
    // no RTCPeerConnection → supported() false → no connect() at load
    localStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
    },
    _fetchCalls: fetchCalls,
  };
  const sandbox = {
    window: win,
    location: { pathname, hostname },
    localStorage: win.localStorage,
    crypto: { getRandomValues: (a) => a },
    Headers: class {},
    fetch: (url, opts) => {
      fetchCalls.push({ url, opts });
      return Promise.resolve({ ok: true, status: 200, _raw: true });
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
  return win;
}

describe("p2p.js owner-fetch wiring (FIX-B)", () => {
  let win;
  beforeEach(() => {
    win = loadP2P();
  });
  afterEach(() => {
    win = null;
  });

  it("exposes window.ownerFetch", () => {
    assert.equal(typeof win.ownerFetch, "function");
  });

  it("window.ownerFetch IS window.p2pFetchStrict (shared strict transport, not a fork)", () => {
    assert.equal(typeof win.p2pFetchStrict, "function");
    assert.equal(
      win.ownerFetch,
      win.p2pFetchStrict,
      "ownerFetch must be the very same function object as p2pFetchStrict",
    );
  });

  it("window.ownerFetch fails closed (503) on a public origin with no transport", async () => {
    // No RTCPeerConnection → no channel; public host → not LAN → must fail closed.
    const r = await win.ownerFetch("/api/mode", {
      method: "POST",
      body: JSON.stringify({ mode: "self_consumption" }),
    });
    assert.equal(r.ok, false);
    assert.equal(r.status, 503, "public-origin no-channel strict call must fail closed with 503");
  });

  it("window.p2pFetch auto-fails closed for direct state-changing owner API calls on public origins", async () => {
    const r = await win.p2pFetch("/api/owner-access/login/finish?ceremony_token=repro", {
      method: "POST",
      body: "FTW_SENTINEL_WEBAUTHN_BODY",
    });
    assert.equal(r.ok, false);
    assert.equal(r.status, 503);
    assert.deepEqual(win._fetchCalls, [], "owner ceremony body must not be raw-fetched to the relay");
  });

  it("window.p2pFetch auto-fails closed for direct state-changing control API calls on public origins", async () => {
    const r = await win.p2pFetch("/api/mode", {
      method: "POST",
      body: JSON.stringify({ mode: "self_consumption" }),
    });
    assert.equal(r.ok, false);
    assert.equal(r.status, 503);
    assert.deepEqual(win._fetchCalls, [], "state-changing owner/control API calls must not hit relay fallback");
  });

  it("window.p2pFetch keeps read-only relay fallback for non-strict API calls", async () => {
    const r = await win.p2pFetch("/api/status");
    assert.equal(r._raw, true);
    assert.equal(win._fetchCalls.length, 1);
    assert.equal(win._fetchCalls[0].url, "/api/status");
  });

  it("window.p2pFetch may raw-fetch state-changing API calls on genuine LAN origins", async () => {
    win = loadP2P({ hostname: "192.168.1.42" });
    const r = await win.p2pFetch("/api/mode", {
      method: "POST",
      body: JSON.stringify({ mode: "self_consumption" }),
    });
    assert.equal(r._raw, true);
    assert.equal(win._fetchCalls.length, 1);
    assert.equal(win._fetchCalls[0].url, "/api/mode");
  });

  it("p2p.js source assigns ownerFetch from p2pFetchStrict (no duplicated logic)", () => {
    assert.match(
      P2P_SRC,
      /window\.ownerFetch\s*=\s*p2pFetchStrict\s*;/,
      "the wiring must be a direct alias of p2pFetchStrict, not a second implementation",
    );
  });

  it("ignores stale browser-local P2P-off toggles on the public owner route", () => {
    assert.match(
      P2P_SRC,
      /localStorage\.getItem\("ftw\.p2p"\)\s*===\s*"off"[\s\S]*?localStorage\.removeItem\("ftw\.p2p"\)/,
      "a stale ftw.p2p=off from older UI must be cleared, not leave home stuck on the connecting gate",
    );
  });
});
