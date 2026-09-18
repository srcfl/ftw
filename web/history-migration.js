// Shared by the startup page, update dialog and live dashboard.
const count = value => Math.max(0, Number.isFinite(Number(value)) ? Number(value) : 0);
const number = value => count(value).toLocaleString("en-US");
const decimal = value => value.toLocaleString("en-US", { maximumFractionDigits: 1 });
const megabytes = value => `${decimal(count(value) / 1e6)} MB`;
function duration(seconds) {
  seconds = Math.max(0, Math.round(seconds));
  if (seconds < 60) return `${seconds} s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} min ${seconds % 60} s`;
  return `${Math.floor(minutes / 60)} h ${minutes % 60} min`;
}
const escape = value => String(value ?? "").replace(/[&<>"']/g, c => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
})[c]);

export function migrationView(migration, { boot = false, connected = true, writer = null, now = Date.now() } = {}) {
  if (!migration || migration.history_complete === true) return null;
  const failed = migration.state === "failed";
  const writeBlocked = connected && count(writer?.pending_ticks) > 0 && !!writer?.last_error;
  const archive = migration.phase === "parquet";
  const seed = migration.phase === "seed";
  const total = count(archive || seed ? migration.current_source_rows_total : migration.rows_total);
  const done = count(archive || seed ? migration.current_source_rows_done : migration.rows_done);
  const byteTotal = archive ? count(migration.source_bytes_total) : 0;
  const byteDone = Math.min(count(migration.source_bytes_done), byteTotal);
  const progress = byteTotal > 0 ? { value: byteDone, max: byteTotal, label: "Older archive data processed" }
    : total > 0 ? { value: Math.min(done, total), max: total, label: archive ? "Current archive file" : seed ? "Current preparation step" : "History import progress" } : null;
  const details = [];
  if (seed && done > 0) details.push(`${number(done)}${total > 0 ? ` of ${number(total)}` : ""} saved items copied in this step`);
  else if (total > 0) details.push(`${archive ? "Current archive: " : ""}${number(done)} of ${number(total)} readings imported`);
  if (!byteTotal && (archive || !total) && count(migration.rows_done) > 0) details.push(`${number(migration.rows_done)} readings imported in total`);
  if (count(migration.files_total) > 0) details.push(`${number(migration.files_done)} of ${number(migration.files_total)} archive files complete`);
  const from = Number(migration.incomplete_from_ms), until = Number(migration.incomplete_until_ms);
  const coverage = Number.isFinite(from) && Number.isFinite(until) && from > 0 && until >= from
    ? `History may be incomplete from ${new Date(from).toLocaleDateString("en-GB")} to ${new Date(until).toLocaleDateString("en-GB")}.`
    : "Older history is not complete yet.";
  const updated = Number(migration.updated_at_ms);
  const age = Number.isFinite(updated) && updated > 0 ? Math.max(0, Math.floor((now - updated) / 1000)) : null;
  const elapsed = age < 60 ? age : Math.floor(age / 60);
  const unit = age < 60 ? "second" : "minute";
  const activity = age === null ? "Waiting for the first import report."
    : `Last import report ${elapsed} ${unit}${elapsed === 1 ? "" : "s"} ago.`;
  const steps = {
    checking: archive ? "Checking saved archive files" : "Checking saved history",
    importing: archive ? "Importing archive readings" : "Importing saved readings",
    waiting_for_live: "Waiting for new measurements to be saved",
    checkpointing: "Saving database progress",
  };
  const step = !connected || writeBlocked ? "" : steps[migration.activity] || "";
  const metrics = [];
  if (byteTotal > 0) metrics.push(`${migration.bytes_estimated ? "About " : ""}${megabytes(byteDone)} of ${megabytes(byteTotal)} processed · ${megabytes(byteTotal - byteDone)} remaining`);
  const rate = count(migration.bytes_per_second);
  const moving = connected && !failed && !writeBlocked && (age === null || age <= 30) && migration.activity === "importing";
  if (byteTotal > 0 && moving && rate > 0) metrics.push(`Speed: ${rate >= 1e6 ? megabytes(rate) : `${decimal(rate / 1e3)} KB`}/s of archive data`);
  const started = count(migration.started_at_ms);
  const elapsedMS = migration.elapsed_ms == null
    ? started > 0 ? Math.max(0, (connected && !failed ? now : count(updated) || started) - started) : 0
    : count(migration.elapsed_ms);
  if (started > 0 || migration.elapsed_ms != null) metrics.push(`Elapsed: ${duration(elapsedMS / 1000)}`);
  const eta = Number(migration.eta_seconds);
  const estimate = !connected || failed || writeBlocked ? "Time remaining unavailable"
    : byteTotal > 0 && moving && rate > 0 && migration.eta_seconds != null && Number.isFinite(eta) && eta >= 0
      ? `Estimated time remaining: about ${duration(Math.max(1, eta))}`
      : "Time remaining: estimating…";
  metrics.push(estimate);
  return {
    title: !connected ? "History import status unavailable" : writeBlocked ? "History import blocked by a database error" : failed ? "History import paused" : boot ? "Preparing FTW" : "Importing older history",
    description: !connected ? "The box is not responding. Showing its last import report."
      : writeBlocked ? "New history readings are not being saved. Import is waiting for database writes to recover."
      : failed ? "The import needs attention. Your original history files are still kept."
      : boot ? "FTW is preparing the data it needs to start. Control has not started yet."
      : "Core is running while FTW imports older readings in the background.",
    details: details.join(" · "), coverage, activity, progress, step,
    metrics: metrics.join(" · "),
    sizeNote: byteTotal > 0 ? `Sizes refer to compressed archive files${migration.bytes_estimated ? "; the current file is estimated" : ""}.` : "",
    error: writeBlocked ? String(writer.last_error).split("\n")[0] : failed ? migration.last_error || "The box could not finish importing history." : "",
    guidance: writeBlocked ? "FTW is retrying the failed write. Keep the box powered and keep the original data and backup. If this error persists, report it with the version shown in Settings."
      : failed || !connected ? "Keep the original data and backup. Reload this page to check the current status."
      : "Keep the box powered. You can close this page and return later. Import progress is saved so it can resume after a restart.",
  };
}

export function migrationHTML(migration, options) {
  const view = migrationView(migration, options);
  if (!view) return "";
  const style = 'style="width:100%;accent-color:var(--accent-e,#fbbf24)"';
  const progress = view.progress
    ? `<progress aria-label="${view.progress.label}" max="${view.progress.max}" value="${view.progress.value}" ${style}></progress>`
    : `<progress aria-label="History import progress" ${style}></progress>`;
  const heading = options?.boot ? "h1" : "h3";
  return `<section class="history-import" aria-label="History import">
    <${heading} role="status">${escape(view.title)}</${heading}><p>${escape(view.description)}</p>
    ${view.step ? `<p>${escape(view.step)}</p>` : ""}
    ${progress}<p>${escape(view.metrics)}</p>${view.sizeNote ? `<p class="dim">${escape(view.sizeNote)}</p>` : ""}
    <p>${escape(view.details)}</p><p>${escape(view.coverage)}</p>
    <p class="dim">${escape(view.activity)}</p>
    ${view.error ? `<p class="err">${escape(view.error)}</p>` : ""}
    <p class="dim">${escape(view.guidance)}</p></section>`;
}

let lastMigration = null;
let lastHealth = null;
export function updateMigrationBanner(health) {
  const current = health?.history_storage?.migration || health?.migration;
  if (current && (current.history_complete === true || !lastMigration || count(current.updated_at_ms) >= count(lastMigration.updated_at_ms))) lastMigration = current;
  if (health) lastHealth = health;
  const view = migrationView(lastMigration, { boot: lastHealth?.status === "starting", connected: !!health, writer: health?.history_storage?.writer });
  let banner = document.getElementById("history-import-banner");
  if (!view) { banner?.remove(); return; }
  if (!banner) {
    const main = document.querySelector("main");
    if (!main) return;
    banner = document.createElement("section");
    banner.id = "history-import-banner";
    banner.className = "storage-banner";
    banner.setAttribute("aria-label", "History import");
    banner.style.cssText = "display:block;text-align:left;padding:16px 24px";
    banner.innerHTML = '<strong data-field="title" role="status"></strong><p data-field="description"></p><p data-field="step"></p><progress aria-label="History import progress" style="width:min(100%,420px);accent-color:var(--amber)"></progress><p data-field="metrics"></p><p data-field="sizeNote"></p><p data-field="details"></p><p data-field="coverage"></p><p data-field="activity"></p><p data-field="error"></p><p data-field="guidance"></p>';
    main.parentNode.insertBefore(banner, main);
  }
  for (const key of ["title", "description", "step", "metrics", "sizeNote", "details", "coverage", "activity", "error", "guidance"]) {
    const element = banner.querySelector(`[data-field="${key}"]`);
    if (element.textContent !== view[key]) element.textContent = view[key];
    element.hidden = !view[key];
  }
  const progress = banner.querySelector("progress");
  progress.setAttribute("aria-label", view.progress?.label || "History import progress");
  if (view.progress) { progress.max = view.progress.max; progress.value = view.progress.value; }
  else progress.removeAttribute("value");
}
