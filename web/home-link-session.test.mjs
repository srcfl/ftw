import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

await import("./home-link-session.js");
const session = globalThis.FTWHomeLinkSession;

function encoded(length, fill) {
  return session.bytesToRawURL(new Uint8Array(length).fill(fill));
}

describe("Home Link browser session", () => {
  it("accepts one canonical invite", () => {
    const invite = session.parseInvite(
      "https://home.sourceful.energy/home-link.html?gateway=001122334455667788" +
      "&route=" + encoded(18, 7) + "&key=" + encoded(64, 8)
    );
    assert.equal(invite.gateway, "001122334455667788");
    assert.equal(invite.route, encoded(18, 7));
    assert.equal(invite.key, encoded(64, 8));
  });

  it("rejects padded and malformed invite values", () => {
    assert.throws(() => session.rawURLToBytes("AA=="));
    assert.throws(() => session.parseInvite(
      "https://home.sourceful.energy/home-link.html?gateway=ABC&route=x&key=y"
    ));
  });

  it("reads one pairing from the fragment without putting the secret in the query", () => {
    const id = encoded(24, 5);
    const secret = encoded(32, 6);
    const pairing = session.parsePairing(
      "https://home.sourceful.energy/home-link.html?gateway=001122334455667788" +
      "#pairing_id=" + id + "&pairing_secret=" + secret + "&label=Fredde%27s%20Mac"
    );
    assert.deepEqual(pairing, {
      id: id, secret: secret, label: "Fredde's Mac",
    });
    assert.equal(session.parsePairing(
      "https://home.sourceful.energy/home-link.html?pairing_secret=" + secret
    ), null);
  });

  it("rejects malformed or unsafe pairing fragments", () => {
    const id = encoded(24, 5);
    const secret = encoded(32, 6);
    assert.throws(() => session.parsePairing(
      "https://home.sourceful.energy/home-link.html#pairing_id=" + id +
      "&pairing_secret=" + secret + "=&label=Browser"
    ));
    assert.throws(() => session.parsePairing(
      "https://home.sourceful.energy/home-link.html#pairing_id=" + id +
      "&pairing_secret=" + secret + "&label=Browser%E2%80%AE"
    ));
    assert.throws(() => session.parsePairing(
      "https://home.sourceful.energy/home-link.html#pairing_id=" + id +
      "&pairing_secret=" + secret + "&label=" + encodeURIComponent("å".repeat(41))
    ));
  });

  it("removes every pairing fragment before the page validates it", () => {
    const malformed = "https://home.sourceful.energy/home-link.html?gateway=test" +
      "#pairing_id=bad&pairing_secret=must-not-remain&label=";
    assert.equal(
      session.withoutFragment(malformed),
      "/home-link.html?gateway=test"
    );
    const page = readFileSync(
      new URL("./home-link.html", import.meta.url), "utf8"
    );
    assert.ok(
      page.indexOf("history.replaceState") < page.indexOf("parsePairing(sourceURL)"),
      "the secret must be scrubbed before pairing validation"
    );
    assert.ok(
      page.indexOf("history.replaceState") <
        page.indexOf('<script src="/home-link-session.js'),
      "the secret must be scrubbed even when the external script does not load"
    );
  });

  it("builds the byte-exact signed transcript", () => {
    const accept = {
      connection_id: "connection",
      gateway_id: "001122334455667788",
      route_generation: 4,
      route_handle: "route",
      stream_id: "stream",
      session_id: "session",
      browser_key: "browser-key",
      gateway_ephemeral_key: "ephemeral",
      gateway_public_key: "gateway-key",
      browser_nonce: "browser-nonce",
      gateway_nonce: "gateway-nonce",
      expires_at_ms: 12345,
    };
    assert.equal(new TextDecoder().decode(session.transcript(accept)), [
      "ftw-home-link-session-accept/v1",
      "connection", "001122334455667788", "4", "route", "stream",
      "session", "browser-key", "ephemeral", "gateway-key",
      "browser-nonce", "gateway-nonce", "12345",
    ].join("\n"));
  });

  it("binds assertions to the exact RP and local credential allow-list", () => {
    const requestID = encoded(16, 1);
    const challenge = {
      version: 1,
      type: "assertion.challenge",
      request_id: requestID,
      challenge_id: encoded(24, 2),
      challenge: encoded(32, 3),
      rp_id: "home.sourceful.energy",
      allow_credentials: [encoded(32, 4)],
      user_verification: "required",
    };
    const publicKey = session.assertionPublicKey(challenge, requestID);
    assert.equal(publicKey.rpId, "home.sourceful.energy");
    assert.equal(publicKey.userVerification, "required");
    assert.equal(publicKey.allowCredentials.length, 1);
    assert.deepEqual(
      Array.from(publicKey.allowCredentials[0].id),
      Array.from(session.rawURLToBytes(challenge.allow_credentials[0]))
    );

    for (const mutate of [
      value => { value.rp_id = "sourceful.energy"; },
      value => { value.rp_id = "other.example"; },
      value => { value.user_verification = "preferred"; },
      value => { value.allow_credentials = []; },
      value => { value.allow_credentials = null; },
      value => { value.allow_credentials = [encoded(32, 4), encoded(32, 4)]; },
      value => { value.allow_credentials = ["AA=="]; },
      value => { value.challenge_id = "AA=="; },
      value => { value.challenge = encoded(31, 3); },
      value => { value.extra = true; },
    ]) {
      const changed = structuredClone(challenge);
      mutate(changed);
      assert.throws(() => session.assertionPublicKey(changed, requestID));
    }
  });

  it("validates the full registration challenge before WebAuthn", () => {
    const requestID = encoded(16, 5);
    const challenge = {
      version: 1,
      type: "registration.challenge",
      request_id: requestID,
      expectation_id: encoded(24, 6),
      challenge: encoded(32, 7),
      rp_id: "home.sourceful.energy",
      user_handle: encoded(32, 8),
      algorithm: -7,
      attestation: "none",
      user_verification: "required",
    };
    const publicKey = session.registrationPublicKey(challenge, requestID);
    assert.equal(publicKey.rp.id, "home.sourceful.energy");
    assert.equal(publicKey.pubKeyCredParams[0].alg, -7);
    assert.equal(publicKey.attestation, "none");
    assert.equal(publicKey.authenticatorSelection.userVerification, "required");

    for (const mutate of [
      value => { value.rp_id = "sourceful.energy"; },
      value => { value.algorithm = -8; },
      value => { value.attestation = "direct"; },
      value => { value.user_verification = "preferred"; },
      value => { value.expectation_id = encoded(23, 6); },
      value => { value.challenge = encoded(31, 7); },
      value => { value.user_handle = encoded(31, 8); },
      value => { delete value.user_handle; },
      value => { value.extra = true; },
    ]) {
      const changed = structuredClone(challenge);
      mutate(changed);
      assert.throws(() => session.registrationPublicKey(changed, requestID));
    }
  });

  it("bounds inbound frames and the pending message queue", async () => {
    assert.deepEqual(session.parseInboundFrame('{"version":1}'), { version: 1 });
    assert.throws(() => session.parseInboundFrame(new Uint8Array([1, 2, 3])));
    assert.throws(() => session.parseInboundFrame("x".repeat(256 * 1024 + 1)));

    let closes = 0;
    const instance = new session.HomeLinkSession(
      { gateway: "001122334455667788", route: encoded(18, 1), key: encoded(64, 2) },
      null,
      10
    );
    instance.socket = { close: () => { closes++; } };
    for (let index = 0; index < 5; index++) {
      instance._push({ index });
    }
    assert.equal(instance.pending.length, 0);
    assert.equal(closes, 1);

    const waiting = new session.HomeLinkSession(
      { gateway: "001122334455667788", route: encoded(18, 1), key: encoded(64, 2) },
      null,
      10
    );
    waiting.socket = { close: () => { closes++; } };
    await assert.rejects(waiting._next(), /timed out/);
    assert.equal(closes, 2);
  });
});
