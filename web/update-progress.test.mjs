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
  assert.match(badge, /case "checking":\s+return "Checking service health"/);
  assert.match(badge, /This step:/);
  assert.match(badge, /Total:/);
  assert.match(badge, /Database schema unchanged; full history backup not needed/);
});

test("setup keeps polling when a safe update takes longer", () => {
  assert.match(setup, /SNAPSHOT_SOFT_TIMEOUT_MS = 15 \* 60 \* 1000/);
  assert.match(setup, /timed_out: true/);
  assert.doesNotMatch(setup, /this\._stopPolling\(\);\s+this\._phase = "timedOut"/);
  assert.match(setup, /case "snapshotting": return "Creating backup"/);
  assert.match(setup, /case "checking":\s+return "Checking service health"/);
});

test("Core image sets ownership during copy without a duplicate app layer", () => {
  assert.match(dockerfile, /COPY --from=builder --chown=100:101 \/out\/ftw\s+\/app\/ftw/);
  assert.match(dockerfile, /COPY --chown=100:101 drivers\/\s+\/app\/drivers\//);
  assert.match(dockerfile, /COPY --chown=100:101 web\/\s+\/app\/web\//);
  assert.doesNotMatch(dockerfile, /chown -R 100:101 \/app/);
});
