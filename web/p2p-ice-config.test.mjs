// node --test web/p2p-ice-config.test.mjs
//
// Public home route reachability guard: p2p.js must ask the relay for ICE
// config before creating RTCPeerConnection so operators can add TURN without
// shipping a new dashboard bundle. TURN still only relays WebRTC ciphertext.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const __dirname = dirname(fileURLToPath(import.meta.url));
const P2P_SRC = readFileSync(join(__dirname, "p2p.js"), "utf8");

async function loadP2PWithICE(iceResponse, opts) {
  opts = opts || {};
  const configs = [];
  const fetches = [];
  const store = new Map();
  function FakeRTCPeerConnection(config) {
    configs.push(config);
    this.iceGatheringState = "complete";
    this.connectionState = "new";
  }
  FakeRTCPeerConnection.prototype.createDataChannel = function () {
    return {
      readyState: "connecting",
      close() {},
      send() {},
    };
  };
  FakeRTCPeerConnection.prototype.addEventListener = function () {};
  FakeRTCPeerConnection.prototype.close = function () {};
  FakeRTCPeerConnection.prototype.createOffer = async function () {
    return { type: "offer", sdp: "v=0\r\n" };
  };
  FakeRTCPeerConnection.prototype.setLocalDescription = async function (offer) {
    this.localDescription = offer;
  };

  const win = {
    RTCPeerConnection: FakeRTCPeerConnection,
    localStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
      removeItem: (k) => store.delete(k),
    },
  };
  const sandbox = {
    window: win,
    RTCPeerConnection: FakeRTCPeerConnection,
    location: { pathname: "/", hostname: "home.fortytwowatts.com", origin: "https://home.fortytwowatts.com" },
    fetch: opts.fetchImpl || (async (url) => {
      fetches.push(String(url));
      return iceResponse;
    }),
    crypto: { getRandomValues: (b) => (b.fill(7), b) },
    localStorage: win.localStorage,
    setTimeout,
    clearTimeout,
    AbortController,
    console: { warn() {} },
  };

  vm.runInNewContext(P2P_SRC, sandbox, { filename: "p2p.js" });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  if (opts.waitMs) await new Promise((resolve) => setTimeout(resolve, opts.waitMs));
  return { configs, fetches };
}

describe("p2p ICE config", () => {
  it("uses relay-provided STUN/TURN config before creating the peer", async () => {
    const { configs, fetches } = await loadP2PWithICE({
      ok: true,
      json: async () => ({
        ice_servers: [
          { urls: ["stun:relay.example:19302"] },
          {
            urls: ["turn:relay.example:3478?transport=udp"],
            username: "1234567890",
            credential: "derived-secret",
          },
        ],
      }),
    });

    assert.deepEqual(fetches, ["/signal/ice"]);
    assert.equal(configs.length, 1);
    assert.deepEqual(JSON.parse(JSON.stringify(configs[0].iceServers)), [
      { urls: ["stun:relay.example:19302"] },
      {
        urls: ["turn:relay.example:3478?transport=udp"],
        username: "1234567890",
        credential: "derived-secret",
      },
    ]);
  });

  it("falls back to default STUN when the relay lacks /signal/ice", async () => {
    const { configs } = await loadP2PWithICE({
      ok: false,
      status: 404,
      json: async () => {
        throw new Error("should not parse 404 body");
      },
    });

    assert.equal(configs.length, 1);
    assert.deepEqual(JSON.parse(JSON.stringify(configs[0].iceServers)), [{ urls: ["stun:stun.l.google.com:19302"] }]);
  });

  // Regression: a /signal/ice that connects but NEVER responds must not hang
  // connect() forever (which would poison every future attempt). The fetch is
  // AbortController-capped at ICE_FETCH_TIMEOUT_MS; on abort it falls back to
  // default STUN and the peer is still created.
  it("does not hang when /signal/ice never responds — aborts and falls back to default STUN", async () => {
    const fetches = [];
    const { configs } = await loadP2PWithICE(null, {
      waitMs: 3300, // > ICE_FETCH_TIMEOUT_MS (3000) so the abort timer fires
      fetchImpl: (url, init) =>
        new Promise((_resolve, reject) => {
          fetches.push(String(url));
          // Reject only when the caller's AbortController fires; otherwise hang.
          if (init && init.signal) {
            init.signal.addEventListener("abort", () => reject(new Error("aborted")));
          }
        }),
    });

    assert.equal(configs.length, 1, "peer should still be created after the ICE fetch aborts");
    assert.deepEqual(JSON.parse(JSON.stringify(configs[0].iceServers)), [{ urls: ["stun:stun.l.google.com:19302"] }]);
  });
});
