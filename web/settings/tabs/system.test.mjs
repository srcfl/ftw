// node --test web/settings/tabs/system.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./system.js");
const { optimizerStatus } = globalThis.window.FTWSettings.tabs.system._pure;

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
