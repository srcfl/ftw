import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

const lanAuth = readFileSync(new URL("./lan-auth.js", import.meta.url), "utf8");
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");

describe("lan-auth UI", () => {
  it("does not store the house password in localStorage", () => {
    assert.doesNotMatch(lanAuth, /localStorage/);
    assert.doesNotMatch(lanAuth, /sessionStorage/);
    const start = system.indexOf('var enableBtn = document.getElementById("sys-lan-auth-enable")');
    const end = system.indexOf('var disableBtn = document.getElementById("sys-lan-auth-disable")', start);
    assert.ok(start >= 0 && end > start, "LAN password handler must be present");
    assert.doesNotMatch(system.slice(start, end), /localStorage|sessionStorage/);
  });

  it("logs in through POST /api/auth/login", () => {
    assert.match(lanAuth, /\/api\/auth\/login/);
    assert.match(lanAuth, /JSON\.stringify\(\s*\{\s*password:/);
    assert.match(index, /lan-auth\.js/);
  });

  it("turns the lock from Settings with /api/auth/password, not the config checkbox", () => {
    assert.match(system, /\/api\/auth\/password/);
    assert.match(system, /LAN password/);
    assert.doesNotMatch(system, /data-path="api\.lan_auth"/);
  });
});
