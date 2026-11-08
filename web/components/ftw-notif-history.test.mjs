import assert from "node:assert/strict";
import { it } from "node:test";

// Exercise the real row renderer and bell count without a DOM or network.
let History;
globalThis.HTMLElement = class {};
globalThis.customElements = { define(name, component) {
  if (name === "ftw-notif-history") History = component;
} };
await import("./ftw-notif-history.js");
delete globalThis.HTMLElement;
delete globalThis.customElements;

function history(rows) {
  return Object.assign(Object.create(History.prototype), {
    _fetchRows: async () => rows,
    _failHours: () => 24,
    _updateDot() {},
  });
}

const row = (status, error = "") => ({
  ts_ms: Date.now(), event_type: "owner.added", title: "A new owner", body: "", status, error,
});

// FTW Cloud holds the notice and the box cannot see it leave (#1785): the
// row must not claim it was sent, nor count as a failure on the bell.
it("shows a notice FTW Cloud holds as queued, not sent", async () => {
  const h = history([row("queued")]);
  const html = h._row(row("queued"));
  assert.match(html, /<tr class="queued">/);
  assert.match(html, /queued/);
  assert.doesNotMatch(html, /sent|failed/);
  await h._refreshBadge();
  assert.equal(h._failCount, 0);
});

it("keeps sent and failed rows as they were", async () => {
  const h = history([row("sent"), row("failed", "no phone or service to send to")]);
  assert.match(h._row(row("sent")), /✓ sent/);
  assert.match(h._row(row("failed", "boom")), /✗ failed[\s\S]*boom/);
  await h._refreshBadge();
  assert.equal(h._failCount, 1);
});
