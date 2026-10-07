import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const setup = readFileSync(new URL("./components/ftw-update-check.js", import.meta.url), "utf8");
const dockerfile = readFileSync(new URL("../Dockerfile", import.meta.url), "utf8");

test("update UI resumes work and shows each server phase", () => {
  assert.match(badge, /_resumeUpdateStatus\(\)/);
  assert.match(badge, /phase_started_at/);
  assert.match(badge, /progress_current/);
  assert.match(badge, /progress_total/);
  assert.match(badge, /Unpacking and checking release/);
  assert.match(badge, /This step:/);
  assert.match(badge, /written, total unknown/);
  assert.match(badge, /No new measured progress for/);
  assert.doesNotMatch(badge, /Large history databases can take several minutes/);
  assert.match(badge, /if \(!this\._info\) return this\._versionLoadingHTML\(\)/);
  assert.match(badge, /return this\._versionHTML\(info\)/);
  assert.match(badge, /Total:/);
  assert.doesNotMatch(badge, /rollback point|full history backup/);
});

test("setup names ftw update and offers no update button", () => {
  assert.match(setup, /info\.native === true/);
  assert.match(setup, /<div class="banner-hint">After setup, install it on the machine that runs FTW with <code>ftw update<\/code>\.<\/div>\s+<div class="banner-actions">\s+<button class="btn-skip" data-action="dismiss">Continue<\/button>/);
  assert.doesNotMatch(setup, /\/api\/version\/update|data-action="update"|sidecar_ready/);
});

test("Core image sets ownership during copy without a duplicate app layer", () => {
  assert.match(dockerfile, /COPY --from=builder --chown=100:101 \/out\/ftw\s+\/app\/ftw/);
  assert.match(dockerfile, /COPY --chown=100:101 drivers\/\s+\/app\/drivers\//);
  assert.match(dockerfile, /COPY --chown=100:101 web\/\s+\/app\/web\//);
  assert.doesNotMatch(dockerfile, /chown -R 100:101 \/app/);
});

test("a failed GitHub check retries a few times instead of waiting three hours", () => {
  assert.match(badge, /ERROR_RETRY_DELAYS_MS = \[30 \* 1000, 90 \* 1000, 180 \* 1000\]/);
  assert.match(badge, /_scheduleErrorRetry\(\)/);
  assert.match(badge, /this\._refresh\(true\)/);
});

test("optimizer fallback is visible in the global header", () => {
  assert.match(badge, /Planner fallback active/);
  // The header mark labels itself from the same warning title it shows on
  // hover; header-status-marks.test.mjs drives the rendered states.
  assert.match(badge, /class="mark warning"[^`]*aria-label="\$\{escapeHTML\(warningTitle\)\}"/);
  assert.match(badge, /optimizer\.fallback_reason \|\| optimizer\.health_error/);
});
