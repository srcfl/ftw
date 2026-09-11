import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const diagnostics = readFileSync(new URL("./diagnostics-modal.js", import.meta.url), "utf8");

test("the plan card offers a help report", () => {
  assert.match(index, /id="plan-help-report"/);
  // The label has to name the problem the user has, not the artefact we
  // produce — someone whose battery is misbehaving does not search for
  // "diagnostics".
  assert.match(index, /Something looks wrong\?/);
  assert.match(index, /id="plan-ask-why"/);
});

// One button, one file. The archive leads with the readable report and
// carries the logs behind it, so a user asking for help never has to work
// out which of two downloads we meant.
test("the help-report button downloads the bundle, not just the text", () => {
  assert.match(plan, /getElementById\('plan-help-report'\)/);
  assert.match(plan, /apiFetch\('\/api\/support\/dump'\)/);
  assert.match(plan, /a\.download = 'ftw-help-' \+ stamp \+ '\.zip'/);
});

test("the help-report button reports failure instead of failing silently", () => {
  assert.match(plan, /restore\('Failed: '/);
  assert.match(plan, /btn\.disabled = true/);
});

test("the driver diagnose modal also links the report", () => {
  assert.match(diagnostics, /data-role="report"/);
  assert.match(diagnostics, /"\/api\/support\/report"/);
});
