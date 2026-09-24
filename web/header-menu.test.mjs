import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const header = index.slice(index.indexOf("<header>"), index.indexOf("</header>"));
const drawer = header.slice(header.indexOf('<div class="header-right">'));

// On phones the right cluster folds into the menu. The bell stays in the
// header row beside the menu button, so its count shows and its history
// opens as its own dialog rather than on top of the menu (#1391).
test("the notification bell sits beside the menu button, outside the drawer", () => {
  const bell = header.indexOf("<ftw-notif-history");
  assert.ok(bell > 0, "header has the bell");
  assert.ok(bell < header.indexOf('id="mobile-menu-btn"'), "bell comes before the menu button");
  assert.doesNotMatch(drawer, /<ftw-notif-history/);
});

// Re-running setup replaces the settings, so it lives in Settings and More,
// not one tap away in the header.
test("the header has no setup wizard entry", () => {
  assert.doesNotMatch(header, /setup-wizard-btn|href="\/setup"/);
  assert.match(index, /class="more-action" href="\/setup"/);
});
