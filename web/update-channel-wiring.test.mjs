import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");

test("update dialog exposes stable and beta as a segmented channel control", () => {
  assert.match(badge, /\["stable", "beta"\]/);
  assert.match(badge, /role="group" aria-label="Update channel"/);
  assert.match(badge, /aria-pressed=/);
  assert.match(badge, /grid-auto-columns: minmax\(0, 1fr\)/);
  assert.doesNotMatch(badge, /grid-template-columns: repeat\(3,/);
});

test("local rollback points are distinguished from off-device backup", () => {
  assert.match(badge, /Local rollback points/);
  assert.match(badge, /not SD-card failure/);
  assert.match(badge, /Create rollback point/);
  assert.doesNotMatch(badge, /Skip backup for this update/);
});

test("full backups expose create, verify, download and external-storage status", () => {
  assert.match(badge, /Full backups/);
  assert.match(badge, /Create full backup/);
  assert.match(badge, /verify-backup/);
  assert.match(badge, /download>Download/);
  assert.match(badge, /do not survive SD-card failure/);
});

test("changing channel persists through the local API then forces a probe", () => {
  assert.match(badge, /_postJSON\("\/api\/version\/channel", \{ channel \}\)/);
  assert.match(badge, /this\._refresh\(true\)/);
});

test("Update Center wires independent Optimizer and Driver history actions", () => {
  assert.match(badge, /FTW Update Center/);
  assert.match(badge, /\/api\/components\/history\?limit=20/);
  assert.match(badge, /\/api\/components\/optimizer\/channel/);
  assert.match(badge, /\/api\/components\/optimizer\/update/);
  assert.match(badge, /\/api\/device_repository\/drivers\//);
  assert.match(badge, /\/versions/);
  assert.match(badge, /\/activate/);
});
