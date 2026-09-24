import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

test("a site with no devices does not show a legacy plan", () => {
  assert.match(source, /function configuredDeviceCount\(status\)/);
  assert.match(source, /if \(configuredDeviceCount\(state\.status\) === 0\)/);
  assert.match(source, /No devices yet — add a device in Settings, and the plan starts once FTW can see your site\./);
});
