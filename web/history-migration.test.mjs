import assert from "node:assert/strict";
import test from "node:test";
import { migrationHTML, migrationView } from "./history-migration.js";

const running = { state:"running", phase:"parquet", history_complete:false, files_done:3, files_total:12, rows_done:6000, updated_at_ms:100000 };
test("unknown totals stay indeterminate and incomplete history stays explicit", () => {
  const view = migrationView(running, {now:130000});
  assert.equal(view.progress, null);
  assert.match(view.details, /6,000 readings.*3 of 12/);
  assert.match(view.coverage, /not complete yet/);
  assert.equal(view.activity, "Last import report 30 seconds ago.");
  assert.match(migrationHTML(running), /<progress aria-label="History import progress"><\/progress>/);
});
test("completed history, not a row percentage, removes the notice", () => {
  assert.ok(migrationView({...running, rows_total:6000}));
  assert.equal(migrationView({...running, history_complete:true}), null);
});
test("startup, live background import and lost contact make distinct claims", () => {
  assert.match(migrationView(running, {boot:true}).description, /Control has not started/);
  assert.match(migrationView(running).description, /Core is running/);
  assert.match(migrationView(running, {connected:false}).description, /last import report/);
  assert.doesNotMatch(migrationView(running, {connected:false}).description, /Core is running/);
});
test("failure keeps incomplete coverage and escapes diagnostic text", () => {
  const failed = {...running, state:"failed", last_error:'bad <script>alert("x")</script>'};
  assert.equal(migrationView(failed).title, "History import paused");
  const html = migrationHTML(failed);
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, /not complete yet/);
});
