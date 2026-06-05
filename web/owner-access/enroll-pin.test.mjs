// node --test web/owner-access/enroll-pin.test.mjs
//
// Tests for the "Show enrollment PIN" affordance (Job 3). The module is
// pure ESM and its top-level imports don't touch the DOM until a handler
// fires, so it imports cleanly under node. We assert the public export
// exists and lock in the behaviours the UI promises via source hygiene:
//   - hits GET /api/owner-access/enroll-pin
//   - treats a relay (apiBase()!="") page as LAN-only
//   - treats a 403 as LAN-only
//   - renders a live countdown + a copy button
//   - re-mints when expired

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const SRC = readFileSync(join(__dirname, "enroll-pin.js"), "utf8");
const ENROLL = readFileSync(join(__dirname, "enroll.html"), "utf8");
const INDEX = readFileSync(join(__dirname, "index.html"), "utf8");

describe("enroll-pin module", () => {
  it("exports mountEnrollPin", async () => {
    const mod = await import("./enroll-pin.js");
    assert.equal(typeof mod.mountEnrollPin, "function");
  });

  it("calls the documented LAN-only endpoint", () => {
    assert.match(SRC, /\/api\/owner-access\/enroll-pin/);
  });

  it("treats a relay page (apiBase prefix present) as LAN-only", () => {
    assert.match(SRC, /isRemote/, "must detect remote/relay context");
    assert.match(SRC, /apiBase\(\)\s*!==\s*['"]['"]/,
      "remote = non-empty /me/<id> apiBase prefix");
    assert.match(SRC, /local network only/i,
      "must show a LAN-only notice on a remote page");
  });

  it("treats a 403 from the endpoint as LAN-only", () => {
    assert.match(SRC, /status\s*===\s*403/);
  });

  it("renders a copy button and a live countdown that re-mints on expiry", () => {
    assert.match(SRC, /pin-copy-btn/);
    assert.match(SRC, /clipboard\.writeText/);
    assert.match(SRC, /setInterval\(tick, 1000\)/, "countdown ticks once per second");
    assert.match(SRC, /mint a new PIN/i, "expired state offers a re-mint");
  });

  it("digits use tabular mono per DESIGN.md (asserted via the host pages' CSS)", () => {
    for (const css of [ENROLL, INDEX]) {
      assert.match(css, /\.pin-digits\s*\{[^}]*var\(--mono\)/s);
      assert.match(css, /\.pin-digits\s*\{[^}]*tabular-nums/s);
      assert.match(css, /\.pin-digits\s*\{[^}]*var\(--accent-e\)/s);
    }
  });
});

describe("owner-access pages stop pointing users at the raw PIN endpoint", () => {
  it("enroll.html no longer instructs curling /api/owner-access/enroll-pin or reading logs", () => {
    assert.doesNotMatch(ENROLL, /read the PIN on the Pi at[\s\S]*enroll-pin/i);
    assert.doesNotMatch(ENROLL, /in its logs/i);
    assert.match(ENROLL, /Show enrollment PIN/,
      "the button copy replaces the raw-API instruction");
  });

  it("both owner-access pages mount the PIN panel", () => {
    for (const html of [ENROLL, INDEX]) {
      assert.match(html, /mountEnrollPin\(/);
      assert.match(html, /id="pin-panel"/);
    }
  });
});
