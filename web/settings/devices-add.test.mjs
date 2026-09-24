import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(new URL("./tabs/devices.js", import.meta.url), "utf8");

test("a cloud password is not rendered again in Secrets", () => {
  assert.match(source, /querySelector\('\[data-path="drivers\.' \+ dIdx \+ '\.config\.password"\]'\)/);
  assert.match(source, /secrets = secrets\.filter\(function \(k\) \{ return k !== 'password'; \}\)/);
});

test("every add path scrolls the new device into view and focuses a connection field", () => {
  assert.match(source, /data-device-idx="' \+ idx \+ '"/);
  assert.match(source, /function revealAddedDevice\(idx\)/);
  assert.match(source, /card\.scrollIntoView\(\{ block: "center" \}\)/);
  const calls = source.match(/revealAddedDevice\(/g) || [];
  assert.ok(calls.length >= 4, "catalog, mqtt and modbus adds must reveal the new card");
});
