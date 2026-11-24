import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");
const diagnose = readFileSync(new URL("./diagnose.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const diagnostics = readFileSync(new URL("./diagnostics-modal.js", import.meta.url), "utf8");

function section(html, id) {
  const start = html.indexOf(`id="${id}"`);
  assert.notEqual(start, -1, `${id} is missing`);
  const end = html.indexOf("</main>", start);
  return html.slice(start, end);
}

// Asking for help is about the whole box, so the file sits under More with
// Settings, Setup and Version, not under one view such as the plan.
test("the help report lives under More, not under Plan", () => {
  const more = section(index, "view-more");
  assert.match(more, /id="more-help-btn"/);
  assert.match(more, /<strong>Get help<\/strong>/);
  // The plan card is moved into the Plan view at run time, so check the
  // whole page: the old button under the chart is gone.
  assert.doesNotMatch(index, /plan-help-report|Download a help report/);
  assert.doesNotMatch(plan, /support\/dump|downloadHelpReport/);
});

// One button, one file. The archive leads with the readable report and
// carries the logs behind it, so a user asking for help never has to work
// out which of two downloads we meant.
test("the help button downloads the bundle, not just the text", () => {
  assert.match(diagnose, /getElementById\('more-help-btn'\)/);
  assert.match(diagnose, /apiFetch\('\/api\/support\/dump'\)/);
  assert.match(diagnose, /a\.download = 'ftw-help-' \+ stamp \+ '\.zip'/);
});

test("the help button reports failure instead of failing silently", () => {
  assert.match(diagnose, /restore\('Failed: '/);
  assert.match(diagnose, /btn\.disabled = true/);
});

test("the driver diagnose modal also links the report", () => {
  assert.match(diagnostics, /data-role="report"/);
  assert.match(diagnostics, /"\/api\/support\/report"/);
});
