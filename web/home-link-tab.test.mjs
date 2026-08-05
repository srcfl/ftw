// node --test web/home-link-tab.test.mjs
//
// Settings → Remote gating and enrollment failure visibility (#762).
//
// The tab used to toggle a `hidden` *class* that no stylesheet defines, so
// the passkey setup section stayed visible and clickable while Home Link
// was unusable. Clicking it opened about:blank, failed, and closed the tab
// again — a white flicker — and wrote the error into the status line the
// 5-second poller rewrites, so nothing readable remained. These tests pin
// the repaired shape: DOM hidden property, a precondition before any tab
// opens, and a dedicated error element the poller never touches.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const JS = readFileSync(join(__dirname, "settings", "tabs", "home-link.js"), "utf8");

describe("remote tab gating uses the DOM hidden property (#762)", () => {
  it("never toggles the unstyled hidden class", () => {
    assert.doesNotMatch(JS, /classList\.toggle\(["']hidden["']/,
      "no stylesheet defines a global .hidden rule — the class hides nothing");
    assert.doesNotMatch(JS, /class="hidden"/,
      "initial markup must start hidden via the attribute, not the class");
  });

  it("hides the sections via the hidden property from actionsState()", () => {
    assert.match(JS, /actionsEl\.hidden = !view\.showAdmin/);
    assert.match(JS, /setupEl\.hidden = !view\.showSetup/);
    assert.match(JS, /id="home-link-actions" hidden/);
    assert.match(JS, /id="home-link-setup" hidden/);
  });
});

describe("enrollment refuses before opening a tab (#762)", () => {
  it("checks the invite URL before window.open", () => {
    const handler = JS.slice(JS.indexOf('enrollBtn.addEventListener'));
    const precondition = handler.indexOf("latestStatus.invite_url");
    const open = handler.indexOf("window.open");
    assert.ok(precondition !== -1 && open !== -1 && precondition < open,
      "a tab with nowhere to go opens and closes itself — the white flicker");
  });

  it("writes failures to an element the status poller does not rewrite", () => {
    assert.match(JS, /id="home-link-error"/,
      "errors need their own element; refresh() rewrites the status line every 5 s");
    const catchBlock = JS.slice(JS.indexOf("setupTab.close()"));
    assert.match(catchBlock.slice(0, 200), /showError\(/,
      "the enrollment catch must use the durable error element");
    assert.doesNotMatch(JS, /statusEl\.textContent = "Could not start passkey setup\."/,
      "the old status-line write is the message nobody ever saw");
  });
});
