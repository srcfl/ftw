import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

globalThis.window = {};
await import("./home-link.js");
const pure = globalThis.window.FTWSettings.tabs["home-link"]._pure;

describe("Remote settings", () => {
  it("shows each honest runtime state", () => {
    assert.equal(
      pure.statusText({ identity_ready: false }),
      "Gateway identity is not ready on this host."
    );
    assert.equal(
      pure.statusText({ identity_ready: true, enabled: false }),
      "Remote access is off. Enable it below, save, then restart Core."
    );
    assert.equal(
      pure.statusText({ identity_ready: true, enabled: true, runtime: { connected: true } }),
      "Connected to the Home Link relay."
    );
    assert.equal(
      pure.statusText({
        identity_ready: true, enabled: true,
        runtime: { connected: false, last_error: "connection-failed" },
      }),
      "Trying to reconnect to the Home Link relay."
    );
  });

  it("keeps passkey revocation visible while Remote is off", () => {
    assert.deepEqual(
      pure.actionsState({ identity_ready: true, enabled: false }),
      { showAdmin: true, showSetup: false }
    );
    assert.deepEqual(
      pure.actionsState({ identity_ready: true, enabled: true }),
      { showAdmin: true, showSetup: true }
    );
    assert.deepEqual(
      pure.actionsState({ identity_ready: false, enabled: true }),
      { showAdmin: false, showSetup: false }
    );
  });

  it("checks pairing labels by their UTF-8 bytes before creating a pairing", () => {
    assert.equal(pure.validPairingLabel("My device"), true);
    assert.equal(pure.validPairingLabel("å".repeat(40)), true);
    assert.equal(pure.validPairingLabel("å".repeat(41)), false);
    assert.equal(pure.validPairingLabel(" Browser"), false);
    assert.equal(pure.validPairingLabel("Browser\u202e"), false);
  });

  it("does not create a pairing when the browser blocks the setup tab", () => {
    const source = readFileSync(
      new URL("./home-link.js", import.meta.url), "utf8"
    );
    const popup = source.indexOf('window.open("about:blank", "_blank")');
    const denied = source.indexOf("if (!setupTab)", popup);
    const pairing = source.indexOf('request("/api/home-link/pairing"', popup);
    assert.ok(popup >= 0 && denied > popup && pairing > denied);
    assert.match(source.slice(denied, pairing), /return;/);
  });
});
