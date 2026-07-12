import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");

test("update dialog exposes stable beta and edge as a segmented channel control", () => {
  assert.match(badge, /\["stable", "beta", "edge"\]/);
  assert.match(badge, /role="group" aria-label="Update channel"/);
  assert.match(badge, /aria-pressed=/);
});

test("changing channel persists through the owner-authenticated API then forces a probe", () => {
  assert.match(badge, /_postJSON\("\/api\/version\/channel", \{ channel \}\)/);
  assert.match(badge, /this\._refresh\(true\)/);
});
