// node --test web/settings/tabs/system.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./system.js");
const { optimizerStatus, bundleDisplay } = globalThis.window.FTWSettings.tabs.system._pure;

describe("optimizerStatus", () => {
  it("shows the active Go fallback and its reason", () => {
    const got = optimizerStatus({
      configured: true,
      healthy: false,
      degraded: true,
      runtime: { version: "bundled", transport: "process" },
      active_solver: {
        engine: "go-dp",
        backend: "bellman",
        fallback: true,
        fallback_reason: "python3 not found",
      },
      last_plan_at_ms: 1234,
    });
    assert.equal(got.degraded, true);
    assert.match(got.warning, /Planner fallback active — go-dp \/ bellman/);
    assert.match(got.warning, /python3 not found/);
    assert.equal(got.lastPlanAtMs, 1234);
  });

  it("shows a worker health failure before a fallback plan exists", () => {
    const got = optimizerStatus({
      configured: true,
      healthy: false,
      health_error: "optimizer socket unavailable",
    });
    assert.equal(got.degraded, true);
    assert.match(got.warning, /Optimizer unavailable/);
    assert.match(got.warning, /optimizer socket unavailable/);
  });

  it("keeps explicit Go DP free of optimizer warnings", () => {
    assert.deepEqual(optimizerStatus({ configured: false }), {
      label: "Go DP only",
      degraded: false,
      warning: "",
      lastPlanAtMs: 0,
    });
  });
});

describe("bundleDisplay", () => {
  it("keeps the per-component breakdown for native installs", () => {
    assert.equal(bundleDisplay({ core: { version: "v1.10.0" } }), null);
    assert.equal(bundleDisplay({ bundle: { kind: "something_else" }, core: { version: "v1.10.0" } }), null);
  });

  it("collapses the Home Assistant add-on to one bundled FTW version", () => {
    const got = bundleDisplay({
      core: { version: "v1.10.0-beta.1" },
      optimizer: { configured: true, runtime: { version: "v1.3.2-beta.1" } },
      bundle: { kind: "home_assistant_addon", version: "0.1.0-beta.1" },
    });
    assert.equal(got.ftwVersion, "v1.10.0-beta.1");
    assert.equal(got.bundleVersion, "0.1.0-beta.1");
    assert.match(got.note, /FTW v1\.10\.0-beta\.1 is bundled with the Home Assistant add-on 0\.1\.0-beta\.1/);
    assert.match(got.note, /Home Assistant manages updates and rollback/);
  });

  it("still names a bundled FTW version when the add-on version is unknown", () => {
    const got = bundleDisplay({ bundle: { kind: "home_assistant_addon" } });
    assert.equal(got.ftwVersion, "dev");
    assert.equal(got.bundleVersion, "");
    assert.match(got.note, /bundled with the Home Assistant add-on\./);
  });
});
