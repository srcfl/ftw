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
    assert.match(system, /House password/);
    assert.doesNotMatch(system, /data-path="api\.lan_auth"/);
  });

  it("speaks to a household, not to a developer", () => {
    const section = (from, to) => {
      const start = system.indexOf(from);
      const end = system.indexOf(to, start);
      assert.ok(start >= 0 && end > start, from + " must be present");
      return system.slice(start, end);
    };
    const page = section("<legend>House password</legend>", "</fieldset>");
    const answers = section("function lanStatusText", "function settingsOpen");
    for (const text of [page, answers, lanAuth]) {
      assert.doesNotMatch(text, /loopback|curl|docker compose|Bearer/i);
    }
  });
});
