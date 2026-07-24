// node --test web/settings/tabs/ha.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./ha.js");
const { statusView } = globalThis.window.FTWSettings.tabs.ha._pure;

describe("Home Assistant status", () => {
  it("shows disabled only when saved config is disabled", () => {
    assert.deepEqual(statusView({ enabled: false }, false), {
      className: "ha-status-indicator ha-off",
      text: "○  disabled in config",
    });
  });

  it("shows a failed bridge as enabled but not connected", () => {
    assert.deepEqual(statusView({
      enabled: true,
      connected: false,
      broker: "192.168.1.65:1883",
    }, true), {
      className: "ha-status-indicator ha-warn",
      text: "⚠  enabled but not connected to 192.168.1.65:1883  —  check broker + credentials",
    });
  });

  it("marks a checked but unsaved form without calling it disabled", () => {
    assert.deepEqual(statusView({ enabled: false }, true), {
      className: "ha-status-indicator ha-warn",
      text: "○  unsaved change — Save to enable",
    });
  });

  it("keeps the connected status details", () => {
    assert.deepEqual(statusView({
      enabled: true,
      connected: true,
      broker: "192.168.1.65:1883",
      sensors_announced: 12,
      last_publish_ms: 9_000,
    }, true, 10_000), {
      className: "ha-status-indicator ha-ok",
      text: "● connected to 192.168.1.65:1883  ·  12 sensors  ·  last publish 1s ago",
    });
  });
});
