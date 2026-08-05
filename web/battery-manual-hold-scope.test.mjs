import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const component = readFileSync(new URL("./components/ftw-battery-control.js", import.meta.url), "utf8");
const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");

test("battery planet opens a scoped manual hold", () => {
  assert.match(app, /bc\.open\(d\.name \|\| d\.id \|\| ""\)/);
  assert.match(component, /data-scope="pool"/);
  assert.match(component, /data-scope="driver"/);
  assert.match(component, /body\.driver = this\._formState\.scopeDriver/);
});

test("manual hold status names its scope", () => {
  assert.match(component, /d\.driver \? " · " \+ d\.driver : " · all batteries"/);
  assert.match(component, /data-scope-row/);
});
