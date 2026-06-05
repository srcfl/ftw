// node --test web/public-landing.test.mjs
//
// Public landing (Task Group 6): an anonymous visitor with no decryptable
// directory sees brand + a passkey button + a discreet "Learn more" link, and
// NOTHING about any instance (no count, label, site_id, or Pi key) pre-auth.
// Static markup/CSS guards — the gate logic is covered in landing-gate.test.mjs.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");
const INDEX = read("index.html");
const CSS = read("next.css");

describe("index.html public landing panel", () => {
  it("has a public-landing panel inside the sign-in gate", () => {
    assert.match(INDEX, /signin-gate-landing/,
      "a .signin-gate-landing panel must exist for the anonymous visitor");
  });

  it("offers a passkey/sign-in button on the landing", () => {
    assert.match(INDEX, /id="signin-landing-btn"/,
      "the landing must carry its own passkey button");
  });

  it("offers a discreet 'Learn more' link", () => {
    assert.match(INDEX, /class="signin-gate-learn"/);
    assert.match(INDEX, /Learn more/i);
  });

  it("leaks NO instance data pre-auth (no site_id/pi_pubkey/label/count tokens)", () => {
    // Slice just the landing panel so we only assert about THIS markup.
    const m = /<div class="signin-gate-landing">([\s\S]*?)<\/div>\s*<!-- \/signin-gate-landing -->/.exec(INDEX);
    assert.ok(m, "landing panel must be delimited by the closing comment for this guard");
    const panel = m[1];
    assert.doesNotMatch(panel, /site:/i, "no site_id in the landing markup");
    assert.doesNotMatch(panel, /pi_pubkey|pubkey/i, "no Pi key in the landing markup");
    assert.doesNotMatch(panel, /instance|tenant/i, "no instance/tenant wording pre-auth");
  });
});

describe("next.css drives the public-landing mode", () => {
  it("shows .signin-gate-landing only in data-mode=public-landing", () => {
    assert.match(CSS, /\.signin-gate-landing\s*\{[^}]*display:\s*none/,
      "landing panel hidden by default");
    assert.match(CSS, /\[data-mode="public-landing"\]\s*\.signin-gate-landing\s*\{[^}]*display:\s*block/,
      "shown only when data-mode is public-landing");
  });

  it("hides the connecting line in public-landing mode (no 'reaching your home')", () => {
    assert.match(CSS, /\[data-mode="public-landing"\]\s*\.signin-gate-connecting\s*\{[^}]*display:\s*none/);
  });
});
