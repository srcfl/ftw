import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The header has one status slot and three things that can claim it: the
// connection light, a waiting update, and a degraded optimizer. Two of
// those used to be separate elements hiding each other through a
// body.has-update CSS rule, and the update and the warning shared a single
// amber dot — which a field tester read as "an update is waiting" when the
// optimizer had actually fallen back to the Go solver (#690, #693).
//
// update-badge.js is a plain IIFE, so it loads under a small shim and these
// tests drive the real component and read the markup it actually renders,
// rather than asserting against the source text.

const source = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const appCss = readFileSync(new URL("./app.css", import.meta.url), "utf8");
const styleCss = readFileSync(new URL("./style.css", import.meta.url), "utf8");
const indexHtml = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const appJs = readFileSync(new URL("./app.js", import.meta.url), "utf8");

function loadBadgeClass() {
  let Captured = null;
  const sandbox = {
    HTMLElement: class {
      attachShadow() {
        return {
          innerHTML: "",
          querySelector: () => null,
          querySelectorAll: () => [],
        };
      }
    },
    customElements: { define: (_name, cls) => { Captured = cls; } },
    // The component only reaches for these from methods the tests never
    // drive (polling, the modal); stubbing keeps the load side-effect free.
    fetch: () => new Promise(() => {}),
    setTimeout, clearTimeout, setInterval, clearInterval,
    URL, console, Date, JSON, Math,
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  assert.ok(Captured, "update-badge.js did not register a component class");
  return Captured;
}

const FtwUpdateBadge = loadBadgeClass();

// Build a badge in a given state and return the markup it renders.
function renderBadge({ update = false, degraded = false, connected = true, latest = "" } = {}) {
  const badge = new FtwUpdateBadge();
  if (update) badge._info = { update_available: true, skipped: false, latest };
  if (degraded) {
    badge._components = {
      optimizer: { configured: true, degraded: true, active_solver: { engine: "go-dp", fallback: true }, fallback_reason: "solver timeout" },
    };
  }
  badge._connected = connected;
  badge._render();
  return badge._shadow.innerHTML;
}

describe("header status marks", () => {
  it("shows a lone green dot when connected with nothing pending", () => {
    const html = renderBadge();
    assert.match(html, /class="mark ok"/);
    assert.doesNotMatch(html, /class="mark update"/);
    assert.doesNotMatch(html, /class="mark warning"/);
    assert.doesNotMatch(html, /class="mark offline"/);
  });

  it("draws a download-to-drive mark when an update is waiting", () => {
    const html = renderBadge({ update: true, latest: "v1.9.1" });
    assert.match(html, /class="mark update"/);
    assert.match(html, /title="Core update available: v1\.9\.1"/);
    // The arrow and the drive body are what make it read as "download",
    // not a bare dot — both have to survive.
    assert.match(html, /<path d="M8 1\.7v6\.2"\/>/);
    assert.match(html, /<rect x="1\.8" y="10\.5"/);
    // The "all good" light stands down when something is pending.
    assert.doesNotMatch(html, /class="mark ok"/);
  });

  it("draws a warning triangle when the optimizer is degraded", () => {
    const html = renderBadge({ degraded: true });
    assert.match(html, /class="mark warning"/);
    assert.match(html, /title="Planner fallback active: solver timeout"/);
    assert.match(html, /<path d="M8 2\.3 14\.9 13\.7H1\.1z"\/>/);
    assert.doesNotMatch(html, /class="mark ok"/);
  });

  // The regression this change exists to fix: the warning used to win
  // outright and the update vanished from the badge, title and aria-label.
  it("shows both marks when an update and a degraded optimizer overlap", () => {
    const html = renderBadge({ update: true, degraded: true, latest: "v1.9.1" });
    assert.match(html, /class="mark update"/);
    assert.match(html, /class="mark warning"/);
    assert.match(html, /title="Core update available: v1\.9\.1"/);
    assert.match(html, /title="Planner fallback active: solver timeout"/);
    assert.ok(
      html.indexOf('class="mark update"') < html.indexOf('class="mark warning"'),
      "the update mark should lead, so the pair keeps a stable order",
    );
  });

  it("replaces everything with the offline dot when the connection drops", () => {
    // Both other states are pending, and both are suppressed: whatever they
    // are reporting is the last payload we managed to fetch.
    const html = renderBadge({ update: true, degraded: true, connected: false });
    assert.match(html, /class="mark offline"/);
    assert.doesNotMatch(html, /class="mark update"/);
    assert.doesNotMatch(html, /class="mark warning"/);
    assert.doesNotMatch(html, /class="mark ok"/);
  });

  it("re-renders through the public setConnected app.js calls each tick", () => {
    const badge = new FtwUpdateBadge();
    assert.equal(typeof badge.setConnected, "function");
    badge.setConnected(false);
    assert.match(badge._shadow.innerHTML, /class="mark offline"/);
    badge.setConnected(true);
    assert.match(badge._shadow.innerHTML, /class="mark ok"/);
  });

  it("escapes the optimizer's reason instead of breaking out of the title", () => {
    const badge = new FtwUpdateBadge();
    badge._components = {
      optimizer: { configured: true, healthy: false, error: 'boom" onmouseover="alert(1)' },
    };
    badge._render();
    const html = badge._shadow.innerHTML;
    // The payload survives as inert text; what must not survive is the
    // quote that would close the attribute and start a new one.
    assert.match(html, /&quot; onmouseover=&quot;alert\(1\)/);
    assert.doesNotMatch(html, /onmouseover="/);
  });

  it("only the two announcements animate, and not under reduced motion", () => {
    const styles = new FtwUpdateBadge()._styles();
    assert.match(styles, /\.mark\.update \{[^}]*animation: fade/);
    assert.match(styles, /\.mark\.warning \{[^}]*animation: fade/);
    // A steady state should not compete for the eye.
    assert.doesNotMatch(styles, /\.mark\.ok \{[^}]*animation/);
    assert.match(styles, /@media \(prefers-reduced-motion: reduce\)[\s\S]*?animation: none/);
  });

  it("takes its three colours from the themed palette, not hardcoded hex", () => {
    const styles = new FtwUpdateBadge()._styles();
    assert.match(styles, /\.mark\.update \{[^}]*color: var\(--cyan/);
    assert.match(styles, /\.mark\.warning \{[^}]*color: var\(--amber/);
    assert.match(styles, /\.mark\.ok \{[^}]*color: var\(--green-e/);
    assert.match(styles, /\.mark\.offline \{[^}]*color: var\(--red-e/);
  });
});

describe("the old standalone connection dot is fully retired", () => {
  it("leaves no #conn-status element or styling behind", () => {
    assert.doesNotMatch(indexHtml, /id="conn-status"/);
    assert.doesNotMatch(appCss, /\.conn-status/);
    assert.doesNotMatch(styleCss, /\.conn-status/);
  });

  // The prose explaining why the swap is gone stays; the selector and the
  // class toggle that drove it must not — so assert against the sheet with
  // its comments stripped.
  it("drops the body.has-update swap the two elements used to need", () => {
    const rules = appCss.replace(/\/\*[\s\S]*?\*\//g, "");
    assert.doesNotMatch(rules, /has-update/);
    assert.doesNotMatch(source, /classList\.toggle\("has-update"/);
  });

  it("routes connection state into the badge from app.js", () => {
    assert.match(appJs, /updateBadge\.setConnected\(ok\)/);
    // Guarded, because app.js runs before the deferred component upgrades.
    assert.match(appJs, /typeof updateBadge\.setConnected === "function"/);
  });
});
