// node --test web/components/ftw-pair-card.test.mjs
//
// Structural / lint-style tests for the pair-card v2 component file.
// We can't run the full custom-element under `node --test` without a
// DOM polyfill, but the source file IS plain text — and the things we
// want to lock in (no old ftw-connect references, no stale helpers)
// are detectable with a regex over the source.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const SRC = readFileSync(join(__dirname, "ftw-pair-card.js"), "utf8");
const RENDER = readFileSync(join(__dirname, "ftw-pair-card-render.js"), "utf8");

describe("ftw-pair-card source hygiene (v2 migration)", () => {
  it("no longer references ftw-connect", () => {
    assert.doesNotMatch(SRC, /ftw-connect/i,
      "Phase 1+2 deleted the ftw-connect binary — the dashboard must not mention it");
    assert.doesNotMatch(SRC, /install-ftw-connect/,
      "old install-script URL must not appear");
  });

  it("no longer ships the inline _aiPrompt copy", () => {
    assert.doesNotMatch(SRC, /_aiPrompt\(/,
      "the 1.5 KB inline agent prompt belongs in docs, not the dashboard bundle");
    assert.doesNotMatch(SRC, /copy-ai-prompt-btn/,
      "the matching copy button should be gone too");
  });

  it("imports from the render helpers module", () => {
    assert.match(SRC, /from ["']\.\/ftw-pair-card-render\.js["']/,
      "render helpers must come from the testable module");
    assert.match(SRC, /friendMessage,/);
    assert.match(SRC, /derivePresence,/);
  });

  it("displays the code prominently for the operator to share", () => {
    assert.match(SRC, /class="big-code"/,
      "the 4-digit code must be displayed for copy/share");
    assert.match(SRC, /id="copy-code-btn"/);
    assert.match(SRC, /id="copy-bundle-btn"/);
  });

  it("does NOT host the approval form anymore (friend types code on relay page)", () => {
    assert.doesNotMatch(SRC, /id="approval-input"/,
      "dashboard must not be the place where the operator types the code");
    assert.doesNotMatch(SRC, /id="approval-btn"/,
      "no Allow button — friend approves on their own page");
  });

  it("renders the URL block with a copy button", () => {
    assert.match(SRC, /class="url-block"/);
    assert.match(SRC, /id="copy-url-btn"/);
  });

  it("uses presence dot classes the CSS knows about", () => {
    for (const cls of ["fresh", "recent", "idle", "pending", "dead"]) {
      assert.match(SRC, new RegExp(`\\.dot\\.${cls}\\b`),
        `CSS must define .dot.${cls}`);
    }
  });
});

describe("ftw-pair-card-render module hygiene", () => {
  it("exports every helper the component imports", () => {
    for (const name of [
      "POLL_MS", "FAST_POLL_MS", "FAST_POLL_ROUNDS",
      "escapeHTML", "computeRemaining", "derivePresence", "formatAge",
      "friendMessage",
    ]) {
      assert.match(RENDER, new RegExp(`export (function|const) ${name}\\b`),
        `${name} must be exported`);
    }
  });

  it("escapeHTML covers the canonical XSS-vector entities", () => {
    // Import dynamically to exercise the actual function.
    return import("./ftw-pair-card-render.js").then(({ escapeHTML }) => {
      assert.equal(
        escapeHTML(`<script>alert("xss & 'pwned'")</script>`),
        `&lt;script&gt;alert(&quot;xss &amp; &#39;pwned&#39;&quot;)&lt;/script&gt;`,
      );
    });
  });
});
