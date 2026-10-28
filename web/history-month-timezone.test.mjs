import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  isCalendarMonthKey,
  monthKeyAt,
  runtimeTimezoneFromSchema,
  shiftMonthKey,
} from "./history-month.js";

const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const historyControls = app.slice(
  app.indexOf("// ---- History wrapper: one period selector"),
  app.indexOf("// ---- Live 24h history (battery + SoC)")
);

test("history month follows the box timezone at a UTC month boundary", () => {
  const instant = new Date("2026-11-01T00:30:00Z");
  assert.equal(monthKeyAt(instant, "America/Los_Angeles"), "2026-10");
  assert.equal(monthKeyAt(instant, "Europe/Stockholm"), "2026-11");
});

test("month arithmetic crosses year boundaries without using the browser timezone", () => {
  assert.equal(shiftMonthKey("2026-01", -1), "2025-12");
  assert.equal(shiftMonthKey("2026-12", 1), "2027-01");
  assert.equal(shiftMonthKey("2026-02", 0), "2026-02");
});

test("month keys and runtime timezone are validated at the API boundary", () => {
  assert.equal(isCalendarMonthKey("2024-02"), true);
  assert.equal(isCalendarMonthKey("2024-13"), false);
  assert.equal(monthKeyAt(new Date("2026-01-01T00:00:00Z"), "Not/A_Timezone"), null);
  assert.equal(runtimeTimezoneFromSchema({ runtime_timezone: "Europe/Stockholm" }), "Europe/Stockholm");
  assert.throws(() => runtimeTimezoneFromSchema({ runtime_timezone: "Not/A_Timezone" }));
});

test("the app waits for runtime timezone before requesting any selected month", () => {
  assert.match(historyControls, /var historyState = \{ range: "week", view: "bars" \}/);
  assert.match(historyControls, /import\("\/history-month\.js"\)/);
  assert.match(historyControls, /boundedApiRead\("\/api\/setup\/schema"/);
  assert.match(historyControls, /runtimeTimezoneFromSchema\(schema\)/);
  assert.match(historyControls, /if \(!historySelectionTouched\) setHistoryRange\("month:" \+ currentMonth, "this-month"\)/);
  assert.match(historyControls, /monthKey\(new Date\(\)\)/);
  assert.doesNotMatch(historyControls, /getFullYear\(\)|getMonth\(\)/);
});
