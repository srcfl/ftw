import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// setup-remote.js imports webauthn.js (apiBase reads location.pathname) and the
// vendored QR encoder. Provide just enough DOM to import + exercise the pure
// helpers; the full mount/render path is DOM-heavy and covered by the source
// hygiene assertions below (mirrors enroll-pin.test.mjs style).
globalThis.location = { pathname: "/owner-access/", origin: "http://192.168.1.42" };
globalThis.document = {
  querySelector: () => null, // no <meta name="ftw-rp-id"> by default
};

const { resolveRpId, enrollUrl, drawQR } = await import("./setup-remote.js");

const __dirname = dirname(fileURLToPath(import.meta.url));
const SRC = readFileSync(join(__dirname, "setup-remote.js"), "utf8");
const INDEX = readFileSync(join(__dirname, "index.html"), "utf8");

test("resolveRpId defaults to the documented home RP-ID", () => {
  // No <meta> override → the Pi's OwnerAccessRPID fallback.
  assert.equal(resolveRpId(), "home.fortytwowatts.com");
});

test("resolveRpId honours a <meta name=\"ftw-rp-id\"> override", () => {
  const saved = globalThis.document.querySelector;
  globalThis.document.querySelector = (sel) =>
    sel === 'meta[name="ftw-rp-id"]' ? { getAttribute: () => "home.example.org" } : null;
  try {
    assert.equal(resolveRpId(), "home.example.org");
  } finally {
    globalThis.document.querySelector = saved;
  }
});

test("enrollUrl carries the bootstrap_id in the FRAGMENT (#b=), never the query", () => {
  const u = enrollUrl("home.fortytwowatts.com", "Abc-123_XYZ");
  assert.equal(u, "https://home.fortytwowatts.com/owner-access/enroll.html#b=Abc-123_XYZ");
  // The id is in the fragment, so it is never sent to a server.
  assert.ok(u.includes("#b="), "bootstrap_id must ride the fragment");
  assert.ok(!u.includes("?"), "bootstrap_id must NOT ride the query string");
  // Percent-encoding of fragment-unsafe chars.
  assert.match(enrollUrl("rp.test", "a b#c"), /#b=a%20b%23c$/);
});

test("drawQR returns a non-empty square canvas for a fixed input (offline render)", () => {
  // Minimal canvas + 2d-context fakes — assert the encoder drives the draw calls
  // and the canvas is sized (module count + quiet zone, integer-scaled).
  let fills = 0;
  const ctx = {
    fillStyle: "",
    fillRect: () => { fills++; },
  };
  const made = { width: 0, height: 0, style: {}, getContext: () => ctx };
  const savedDoc = globalThis.document;
  globalThis.document = { ...savedDoc, createElement: (t) => (t === "canvas" ? made : { style: {} }) };
  try {
    const url = enrollUrl("home.fortytwowatts.com", "x".repeat(43));
    const canvas = drawQR(url, 240);
    assert.ok(canvas.width > 0 && canvas.width === canvas.height, "square, sized canvas");
    assert.ok(fills > 10, "drew the background + many dark modules");
  } finally {
    globalThis.document = savedDoc;
  }
});

// ---- source hygiene: lock in the contract the DOM-heavy render path promises ----

test("setup-remote.js reads the {bootstrap_id, pin, expires_in_s} endpoint shape", () => {
  assert.match(SRC, /\/api\/owner-access\/enroll-pin/);
  assert.match(SRC, /bootstrap_id/);
  assert.match(SRC, /expires_in_s/);
});

test("setup-remote.js is LAN-only and degrades on a relay page / 403", () => {
  assert.match(SRC, /apiBase\(\)\s*!==\s*['"]['"]/, "relay page → LAN-only notice");
  assert.match(SRC, /status\s*===\s*403/, "403 → LAN-only notice");
});

test("setup-remote.js renders a QR, a clickable link, the PIN, and a live countdown", () => {
  assert.match(SRC, /qrMatrix|drawQR/, "must encode a QR");
  assert.match(SRC, /setup-link/, "must render a clickable link");
  assert.match(SRC, /pin-digits/, "must render the PIN digits");
  assert.match(SRC, /setInterval\(tick, 1000\)/, "countdown ticks once per second");
  assert.match(SRC, /clipboard\.writeText/, "PIN is copyable");
  assert.match(SRC, /mint a fresh link/i, "expired state re-mints");
});

test("setup-remote.js is display-only — it runs NO WebAuthn (plain HTTP page)", () => {
  assert.doesNotMatch(SRC, /navigator\.credentials/, "no WebAuthn on the LAN HTTP page");
});

test("index.html mounts the setup-remote affordance and the QR encoder is vendored locally", () => {
  assert.match(INDEX, /mountSetupRemote\(/);
  assert.match(INDEX, /id="setup-remote-panel"/);
  // No CDN / Google Fonts: the QR encoder is imported from the local vendor dir,
  // not fetched from a remote host (fresh-Pi-without-WAN rule, DESIGN.md).
  assert.match(SRC, /from\s+["']\.\.\/vendor\/qrcode\.js["']/, "QR module is a local vendor import");
  // No remote import/script host — match an actual CDN hostname, not the word "CDN".
  assert.doesNotMatch(SRC, /(googleapis\.com|unpkg\.com|jsdelivr\.net|skypack\.dev|cdnjs)/i, "no CDN host");
});
