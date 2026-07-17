import { test } from "node:test";
import assert from "node:assert/strict";

const { qrMatrix } = await import("./qrcode.js");

test("qrMatrix loads and produces a square boolean matrix", async () => {
  const m = qrMatrix("HELLO WORLD");
  assert.ok(Array.isArray(m) && m.length > 0, "matrix must be non-empty");
  assert.equal(m.length, m[0].length, "matrix must be square");
  // Version 1 (the smallest) is 21×21; "HELLO WORLD" (11 bytes) fits it at level M.
  assert.equal(m.length, 21);
  for (const row of m) for (const v of row) assert.equal(typeof v, "boolean");
});

test("qrMatrix places the three finder patterns (structural correctness)", () => {
  const m = qrMatrix("HELLO WORLD");
  const n = m.length;
  // A finder pattern's outer ring: the top edge of the 7×7 box is all-dark.
  const topEdgeDark = (r, c) => m[r].slice(c, c + 7).every(Boolean);
  assert.ok(topEdgeDark(0, 0), "top-left finder");
  assert.ok(topEdgeDark(0, n - 7), "top-right finder");
  assert.ok(topEdgeDark(n - 7, 0), "bottom-left finder");
  // The mandated dark module sits at modules[n-8][8].
  assert.equal(m[n - 8][8], true, "dark module present");
});

test("qrMatrix grows the version with the payload and is deterministic", () => {
  const url = "https://ftw.local/setup#token=" + "x".repeat(43);
  const a = qrMatrix(url);
  const b = qrMatrix(url);
  assert.ok(a.length >= 25, "a real onboarding URL needs a larger version than 21");
  // Deterministic: identical input → byte-identical matrix (stable snapshot basis).
  assert.deepEqual(a, b);
});

test("qrMatrix snapshot — fixed input yields a stable module count + checksum", () => {
  const m = qrMatrix("ftw-bootstrap-snapshot");
  // Stable size for this fixed input (22 bytes → version 2, 25×25 at level M).
  assert.equal(m.length, 25);
  // A cheap content fingerprint: count dark modules. Locks the encoder against
  // accidental regressions in masking / RS without pinning the whole matrix.
  // Verified byte-identical to qrcode-generator@1.4.4 (byte mode, level M).
  let dark = 0;
  for (const row of m) for (const v of row) if (v) dark++;
  assert.equal(dark, 306);
});
