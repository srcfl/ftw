import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

describe("plan unavailable reason", () => {
  it("feeds the diagnose skip reason into the brief and strategy controls", () => {
    assert.match(plan, /mpcReason: \(m && m\.reason\) \|\| ""/);
    assert.match(plan, /unavailableReason: \(state\.enabled && state\.enabled\.mpcReason\)/);
    assert.match(plan, /applyPlannerModeAvailability/);
    assert.match(plan, /btn\.disabled = !enabled/);
    assert.match(plan, /replan\.disabled = !enabled/);
    assert.match(plan, /usePlan\.disabled = !enabled/);
    assert.doesNotMatch(plan, /MPC planner disabled/);
  });
});
