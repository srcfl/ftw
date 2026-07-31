import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

test("plan renders a visible optimizer fallback alert", () => {
  assert.match(plan, /id = 'plan-optimizer-fallback'/);
  assert.match(plan, /setAttribute\('role', 'alert'\)/);
  assert.match(plan, /Mathematical optimizer unavailable\. This plan uses the built-in Go fallback\./);
  assert.match(plan, /solver\.fallback_reason/);
});
