// node --test web/public-route-fetch-guard.test.mjs
//
// Static regression guard for the public home route (FIX-B). The dashboard is
// served to a friend / over the untrusted relay on the public home host. Every
// STATE-CHANGING owner/CONTROL /api/* call MUST ride window.ownerFetch (the strict,
// fail-closed transport wired in p2p.js to p2pFetchStrict) — NEVER a bare fetch(),
// which on the public home host would send the body + owner session to the relay
// in cleartext.
//
// This guard scans exactly the JS modules index.html pulls in (classic scripts +
// the transitive web-component module graph from components/index.js) and FAILS if
// any of them contains a bare `fetch(...)` whose call carries a state-changing
// method (POST / PUT / DELETE / PATCH). Read-only GETs are allowed — they carry no
// body, and on the P2P-only route the relay strips the owner cookie, so a GET can't
// leak. `/api/identity` is allowlisted: it is the unauthenticated TOFU pin fetch
// p2p.js itself issues, by design, before any channel exists.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const WEB = __dirname;

// ---- discover the public-route module set (what index.html actually loads) ----

// Classic <script src="/..."> tags loaded directly by index.html.
function classicScripts(indexHtml) {
  const out = [];
  const re = /<script\b[^>]*\bsrc=["']\/([^"'?]+\.js)/g;
  let m;
  while ((m = re.exec(indexHtml))) out.push(m[1]);
  return out;
}

// Transitively resolve the static `import "./x.js"` / `import { y } from "./x.js"`
// graph rooted at a module, so a component the page never names directly (but is
// imported by components/index.js) is still covered.
function resolveModuleGraph(entryRel, seen) {
  const abs = resolve(WEB, entryRel);
  if (seen.has(abs)) return;
  seen.add(abs);
  let src;
  try {
    src = readFileSync(abs, "utf8");
  } catch {
    return; // a vendored / missing path — nothing to scan
  }
  const dir = dirname(abs);
  const re = /\bimport\s+(?:[^"']*?\bfrom\s+)?["'](\.\.?\/[^"']+)["']/g;
  let m;
  while ((m = re.exec(src))) {
    const childAbs = resolve(dir, m[1]);
    const childRel = childAbs.slice(WEB.length + 1);
    resolveModuleGraph(childRel, seen);
  }
}

function publicRouteModules() {
  const indexHtml = readFileSync(join(WEB, "index.html"), "utf8");
  const seen = new Set();
  for (const rel of classicScripts(indexHtml)) {
    resolveModuleGraph(rel, seen);
  }
  // Drop the test files themselves and return repo-relative paths, sorted.
  return [...seen]
    .map((abs) => abs.slice(WEB.length + 1))
    .filter((rel) => !rel.endsWith(".test.mjs"))
    .sort();
}

// ---- the bare-state-changing-fetch detector ----

// Allowlisted bare-fetch targets: pure-GET reads carry no body and (on the relay)
// no owner cookie, so they can't leak; /api/identity is the by-design TOFU pin.
const ALLOWED_PATHS = [/^\/api\/identity\b/];

// Match a bare `fetch(` — one NOT preceded by a word char or a dot, so
// ownerFetch( / p2pFetch( / window.ftwP2P.fetch( / something.fetch( are excluded.
const BARE_FETCH = /(^|[^.\w])fetch\s*\(/g;
const STATE_CHANGING_METHOD = /method\s*:\s*["'`](POST|PUT|DELETE|PATCH)\b/i;
// A literal "/api/..." path argument as the FIRST fetch() argument — this is the
// exact shape the task targets: a bare fetch('/api…') / fetch("/api…") /
// fetch(`/api…`). A dynamic-URL fetch (signaling /signal/*, or a var holding a
// non-/api URL) is out of scope: the threat is a literal owner/CONTROL /api call.
const API_PATH_LITERAL = /^\s*["'`](\/api\/[A-Za-z0-9_./-]*)/;

function scanForBareStateChangingFetch(src) {
  const findings = [];
  let m;
  BARE_FETCH.lastIndex = 0;
  while ((m = BARE_FETCH.exec(src))) {
    const open = m.index + m[0].length; // index just after "("
    // Window large enough to span the options object literal that carries `method`.
    const win = src.slice(open, open + 400);
    // First argument must be a literal /api/... path (skip dynamic-URL fetches).
    const pathM = API_PATH_LITERAL.exec(win);
    if (!pathM) continue;
    const path = pathM[1];
    if (ALLOWED_PATHS.some((re) => re.test(path))) continue;
    // The call must carry a state-changing method to be a leak risk.
    if (!STATE_CHANGING_METHOD.test(win)) continue;
    const line = src.slice(0, m.index).split("\n").length;
    findings.push({ line, path });
  }
  return findings;
}

// ---- tests ----

describe("public-route fetch guard (FIX-B)", () => {
  const modules = publicRouteModules();

  it("discovers the public-route module set (sanity: the key files are in it)", () => {
    // A few anchors so a future refactor that silently drops the index.html graph
    // (and would make this guard scan nothing) is caught.
    for (const expected of [
      "next-app.js",
      "settings.js",
      "update-badge.js",
      "components/ftw-battery-control.js",
      "components/ftw-pair-card.js",
      "components/ftw-update-check.js",
    ]) {
      assert.ok(
        modules.includes(expected),
        `expected ${expected} in the public-route module set; got: ${modules.join(", ")}`,
      );
    }
  });

  it("no public-route module bare-fetches a state-changing /api call", () => {
    const offenders = [];
    for (const rel of modules) {
      const src = readFileSync(join(WEB, rel), "utf8");
      for (const f of scanForBareStateChangingFetch(src)) {
        offenders.push(`${rel}:${f.line} → bare fetch(${f.path}) with a state-changing method`);
      }
    }
    assert.deepEqual(
      offenders,
      [],
      "state-changing owner/CONTROL calls must use window.ownerFetch, not bare fetch():\n" +
        offenders.join("\n"),
    );
  });
});

// ---- the detector's own sanity (so a broken regex can't silently pass) ----

describe("bare-state-changing-fetch detector self-check", () => {
  it("flags a bare POST to /api", () => {
    const bad = `fetch("/api/mode", { method: "POST", body: "{}" })`;
    assert.equal(scanForBareStateChangingFetch(bad).length, 1);
  });
  it("flags a bare DELETE to /api", () => {
    const bad = `fetch("/api/battery/manual_hold", { method: "DELETE" })`;
    assert.equal(scanForBareStateChangingFetch(bad).length, 1);
  });
  it("does NOT flag ownerFetch(...) POSTs", () => {
    const ok = `ownerFetch("/api/mode", { method: "POST", body: "{}" })`;
    assert.equal(scanForBareStateChangingFetch(ok).length, 0);
  });
  it("does NOT flag window.ftwP2P.fetch / p2pFetchStrict POSTs", () => {
    const ok = `window.p2pFetchStrict("/api/mode", { method: "POST" })`;
    assert.equal(scanForBareStateChangingFetch(ok).length, 0);
  });
  it("does NOT flag read-only GETs (no method, or method GET)", () => {
    const ok = `fetch("/api/status").then(r => r.json()); fetch("/api/config", { method: "GET" })`;
    assert.equal(scanForBareStateChangingFetch(ok).length, 0);
  });
  it("allowlists /api/identity even on a (hypothetical) bare POST", () => {
    const ok = `fetch("/api/identity", { method: "POST" })`;
    assert.equal(scanForBareStateChangingFetch(ok).length, 0);
  });
});
