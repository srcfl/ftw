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
  assert.match(migrationHTML(running), /<progress(?![^>]*\bvalue=)[^>]*><\/progress>/);
});
test("completed history, not a row percentage, removes the notice", () => {
  assert.ok(migrationView({...running, rows_total:6000}));
  assert.equal(migrationView({...running, history_complete:true}), null);
});
test("startup uses the current seed step count before overall totals exist", () => {
  const view = migrationView({state:"starting", phase:"seed", history_complete:false, rows_done:0, current_source_rows_done:65536}, {boot:true});
  assert.equal(view.details, "65,536 saved items copied in this step");
  assert.equal(view.progress, null);
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
const measured = {...running, activity:"importing", source_bytes_total:500e6, source_bytes_done:200e6, bytes_per_second:250000, bytes_estimated:true, started_at_ms:1000, elapsed_ms:800000, eta_seconds:1200};
test("archive progress shows source MB, rate, elapsed time and a qualified ETA", () => {
  const view = migrationView(measured, {now:110000});
  assert.deepEqual(view.progress, {value:200e6, max:500e6, label:"Older archive data processed"});
  assert.match(view.metrics, /About 200 MB of 500 MB processed · 300 MB remaining/);
  assert.match(view.metrics, /250 KB\/s of archive data/);
  assert.match(view.metrics, /Elapsed: 13 min 20 s/);
  assert.match(view.metrics, /Estimated time remaining: about 20 min 0 s/);
  assert.match(view.sizeNote, /compressed archive files.*current file is estimated/);
});
test("checking, paused and stale progress do not keep advertising a live rate or ETA", () => {
  for (const [data, options] of [
    [{...measured, activity:"checking"}, {now:110000}],
    [{...measured, activity:"waiting_for_live"}, {now:110000}],
    [{...measured, state:"failed"}, {now:110000}],
    [measured, {now:150000}],
    [measured, {now:110000, connected:false}],
  ]) {
    const view = migrationView(data, options);
    assert.doesNotMatch(view.metrics, /Speed:|Estimated time remaining: about/);
  }
  assert.equal(migrationView({...measured, activity:"checking"}).step, "Checking saved archive files");
});
test("unknown byte totals and estimates never become zero remaining", () => {
  const view = migrationView({...running, phase:"sqlite", activity:"checking", started_at_ms:1000}, {now:131000});
  assert.match(view.metrics, /Elapsed: 2 min 10 s/);
  assert.match(view.metrics, /Time remaining: estimating/);
  assert.doesNotMatch(view.metrics, / MB|Speed:/);
  assert.equal(view.sizeNote, "");
  assert.equal(view.progress, null);
});
test("all bytes processed still means verification is pending until Core confirms completion", () => {
  const view = migrationView({...measured, source_bytes_done:600e6, activity:"checkpointing"}, {now:110000});
  assert.equal(view.progress.value, view.progress.max);
  assert.match(view.metrics, /0 MB remaining/);
  assert.match(view.coverage, /not complete yet/);
  assert.equal(view.step, "Saving database progress");
  assert.doesNotMatch(view.metrics, /Speed:|Estimated time remaining: about/);
});
