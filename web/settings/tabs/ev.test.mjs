import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { describe, it } from "node:test";

const source = readFileSync(new URL("./ev.js", import.meta.url), "utf8");

describe("Settings → EV Tesla Wall Connector", () => {
  it("offers tesla-wc next to the cloud providers", () => {
    assert.match(source, /\["easee", "zaptec", "tesla-wc"\]/);
  });

  it("shows a LAN host field for tesla-wc, not cloud email", () => {
    assert.match(source, /ev_charger\.provider", "easee"\) === "tesla-wc"/);
    assert.match(source, /ev_charger\.http\.base_url/);
    assert.match(source, /LAN address of the Wall Connector/);
  });

  it("says the box cannot take a current setpoint", () => {
    assert.match(source, /cannot take a current setpoint/);
    assert.match(source, /tesla_vehicle/);
  });

  it("clears leftover cloud credentials when switching to tesla-wc", () => {
    // captureCurrentTab still sees the Easee email/password fields. If
    // those stay on the config, Validate() rejects tesla-wc and the
    // config API restores ev_charger_password from state.db.
    assert.match(source, /providerSel\.value === "tesla-wc"/);
    assert.match(source, /ev\.username = ""/);
    assert.match(source, /ev\.email = ""/);
    assert.match(source, /ev\.password = ""/);
  });

  it("treats tesla-wc / tesla_wall as a live charger in the status badge", () => {
    assert.match(source, /n\.indexOf\("tesla-wc"\) >= 0/);
    assert.match(source, /n\.indexOf\("tesla_wall"\) >= 0/);
  });
});
