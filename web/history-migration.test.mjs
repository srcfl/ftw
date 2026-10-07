import assert from "node:assert/strict";
import test from "node:test";
import { migrationHTML, migrationView, maintenanceHTML } from "./history-migration.js";

const running = { state:"running", phase:"parquet", history_complete:false, files_done:3, files_total:12, rows_done:6000, updated_at_ms:100000 };

test("maintenance shows saved progress, write freshness and failures without a false percentage", () => {
  const status = { state: "pending", work: { sample_archive: { file: "2026-06-04", operation: "copy_samples", rows_done: 10240 } } };
  const html = maintenanceHTML(status, { last_commit_ms: 99000 }, { now: 100000 });
  assert.match(html, /History work will continue automatically/);
  assert.match(html, /saved readings in the background/);
  assert.match(html, /You do not need to do anything/);
  assert.doesNotMatch(html, /import|Older history will continue/);
  assert.match(html, /10,240 records processed/);
  assert.match(html, /saved the latest measurements 1 s ago/);
  assert.doesNotMatch(html, /%|<progress/);
  assert.match(maintenanceHTML({ ...status, last_error: "disk <failed>" }), /disk &lt;failed&gt;/);
  assert.match(maintenanceHTML(status, null, { connected: false }), /status unavailable/);
  assert.equal(maintenanceHTML({ state: "complete" }), "");
});
test("a routine pending pass with no reported backlog stays quiet after restart", () => {
  const status = { state: "pending", started_ms: 1000, runs: 1, failures: 0, rows_done: 0 };
  assert.equal(maintenanceHTML(status, { last_commit_ms: 99000 }, { now: 100000 }), "");
  assert.equal(maintenanceHTML({ ...status, work: {} }), "");
});
test("pending work retains a known total even before the first row finishes", () => {
  const html = maintenanceHTML({ state: "pending", rows_done: 0, rows_total: 100 });
  assert.match(html, /0 of 100 records processed/);
  assert.match(html, /continue automatically/);
});
test("history errors and failed writes bypass quiet pending and short running passes", () => {
  for (const state of ["pending", "running"]) {
    const status = { state, started_ms: 99000, rows_done: 0 };
    const options = { now: 100000 };
    assert.match(maintenanceHTML({ ...status, last_error: "disk <failed>" }, null, options), /Saved history needs attention/);
    const html = maintenanceHTML(status, { last_error: "disk <full>", last_commit_ms: 99000 }, options);
    assert.match(html, /FTW cannot save new readings/);
    assert.match(html, /disk &lt;full&gt;/);
    assert.doesNotMatch(html, /You do not need to do anything|FTW saved the latest/);
  }
  assert.match(maintenanceHTML({ state: "failed" }), /Saved history needs attention/);
  const both = maintenanceHTML({ state: "pending", last_error: "summary unavailable" }, { last_error: "disk full" });
  assert.match(both, /New readings: disk full/);
  assert.match(both, /Saved history: summary unavailable/);
  for (const state of ["complete", "not_started"]) {
    assert.match(maintenanceHTML({ state }, { last_error: "disk full" }), /FTW cannot save new readings/);
  }
});
test("lost contact shows the last report without claiming fresh writes", () => {
  for (const state of ["pending", "running"]) {
    const html = maintenanceHTML({ state, started_ms: 99000 }, { last_commit_ms: 99000 }, { now: 100000, connected: false });
    assert.match(html, /History status unavailable/);
    assert.match(html, /last history report/);
    assert.doesNotMatch(html, /You do not need to do anything|FTW saved the latest/);
  }
});
test("backup and long running passes explain the task and user action", () => {
  assert.match(maintenanceHTML({ state: "paused" }), /after the backup.*You do not need to do anything/);
  assert.equal(maintenanceHTML({ state: "running", started_ms: 99000 }, null, { now: 100000 }), "");
  const html = maintenanceHTML({ state: "running", started_ms: 1000 }, { last_commit_ms: 99000 }, { now: 100000 });
  assert.match(html, /Organizing saved history/);
  assert.match(html, /keep charts fast and storage within its limits/);
  assert.match(html, /FTW saved the latest measurements 1 s ago/);
});
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

test("a failed live write explains the blocked import and clears after recovery", () => {
  const writer = {pending_ticks:64, last_error:'Out of memory <database>\nSQL tuning advice'};
  const options = {now:110000, writer};
  const view = migrationView(measured, options);
  assert.equal(view.title, "History import blocked by a database error");
  assert.match(view.description, /New history readings are not being saved/);
  assert.equal(view.error, 'Out of memory <database>');
  assert.equal(view.step, "");
  assert.match(view.metrics, /300 MB remaining.*Time remaining unavailable/);
  assert.doesNotMatch(view.metrics, /Speed:|estimating|Estimated time/);
  assert.match(migrationHTML(measured, options), /Out of memory &lt;database&gt;/);
  for (const recovered of [{...writer, last_error:""}, {...writer, pending_ticks:0}]) {
    assert.equal(migrationView(measured, {...options, writer:recovered}).title, "Importing older history");
  }
  const disconnected = migrationView(measured, {...options, connected:false});
  assert.equal(disconnected.title, "History import status unavailable");
  assert.equal(disconnected.error, "");
});
