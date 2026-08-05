import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { qrMatrix } from "./vendor/qrcode.js";

// The encoder produced symbols no reader could decode above type 6.
//
// Two faults, both invisible to the eye: version information was never
// written (mandatory from type 7), and the timing line was laid down before
// the alignment patterns, so the "module already taken" check skipped the
// centres that sit on row 6 and column 6 — (6,22) and (22,6) on a type 7.
// The result looked exactly like a QR code and decoded as nothing.
//
// It stayed hidden because every payload the box had until now was short.
// The calendar link lands on type 4; the app pairing URL is 165 characters
// and lands on type 9.
//
// So this test decodes. Asserting on module counts or on the presence of a
// matrix is what let the bug through in the first place.

/**
 * Decode a boolean module matrix without a camera or a canvas.
 *
 * Reads the format information, undoes the mask, walks the data modules in
 * the standard zig-zag, de-interleaves the Reed-Solomon blocks and reads the
 * byte-mode segment. No error correction: these symbols are generated, not
 * photographed, so any damage is a fault in the encoder and must fail rather
 * than be repaired quietly.
 */
function decode(matrix) {
  const n = matrix.length;
  const type = (n - 17) / 4;
  assert.ok(Number.isInteger(type) && type >= 1, `bad module count ${n}`);

  // ---- format information, from the copy beside the top-left finder ----
  const formatRaw = [];
  for (let i = 0; i < 15; i++) {
    if (i < 6) formatRaw.push(matrix[i][8]);
    else if (i < 8) formatRaw.push(matrix[i + 1][8]);
    else formatRaw.push(matrix[n - 15 + i][8]);
  }
  let format = 0;
  for (let i = 0; i < 15; i++) if (formatRaw[i]) format |= 1 << i;
  format ^= 0x5412; // the mask the standard applies to format info
  const ecLevel = (format >> 13) & 0b11;
  const mask = (format >> 10) & 0b111;
  assert.equal(ecLevel, 0, "expected error-correction level M");

  // ---- which modules carry data ----
  const reserved = Array.from({ length: n }, () => new Array(n).fill(false));
  const reserve = (r0, c0, h, w) => {
    for (let r = r0; r < r0 + h; r++)
      for (let c = c0; c < c0 + w; c++)
        if (r >= 0 && r < n && c >= 0 && c < n) reserved[r][c] = true;
  };
  reserve(0, 0, 9, 9);
  reserve(0, n - 8, 9, 8);
  reserve(n - 8, 0, 8, 9);
  for (let i = 0; i < n; i++) {
    reserved[6][i] = true;
    reserved[i][6] = true;
  }
  if (type >= 7) {
    reserve(0, n - 11, 6, 3);
    reserve(n - 11, 0, 3, 6);
  }
  const alignPos = [
    [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34],
    [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50],
  ][type - 1];
  for (const r of alignPos)
    for (const c of alignPos) {
      const nearFinder =
        (r <= 8 && c <= 8) || (r <= 8 && c >= n - 9) || (r >= n - 9 && c <= 8);
      if (!nearFinder) reserve(r - 2, c - 2, 5, 5);
    }

  // ---- read the data modules, unmasking as we go ----
  const maskFn = [
    (i, j) => (i + j) % 2 === 0,
    (i) => i % 2 === 0,
    (i, j) => j % 3 === 0,
    (i, j) => (i + j) % 3 === 0,
    (i, j) => (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0,
    (i, j) => ((i * j) % 2) + ((i * j) % 3) === 0,
    (i, j) => (((i * j) % 2) + ((i * j) % 3)) % 2 === 0,
    (i, j) => (((i * j) % 3) + ((i + j) % 2)) % 2 === 0,
  ][mask];

  const bits = [];
  let upward = true;
  for (let col = n - 1; col > 0; col -= 2) {
    if (col === 6) col--;
    for (let k = 0; k < n; k++) {
      const row = upward ? n - 1 - k : k;
      for (const c of [col, col - 1]) {
        if (reserved[row][c]) continue;
        bits.push(matrix[row][c] !== maskFn(row, c));
      }
    }
    upward = !upward;
  }

  const bytes = [];
  for (let i = 0; i + 7 < bits.length; i += 8) {
    let b = 0;
    for (let j = 0; j < 8; j++) if (bits[i + j]) b |= 1 << (7 - j);
    bytes.push(b);
  }

  // ---- de-interleave the Reed-Solomon blocks ----
  const layout = [
    [[1, 26, 16]], [[1, 44, 28]], [[1, 70, 44]], [[2, 50, 32]], [[2, 67, 43]],
    [[4, 43, 27]], [[4, 49, 31]], [[2, 60, 38], [2, 61, 39]],
    [[3, 58, 36], [2, 59, 37]], [[4, 69, 43], [1, 70, 44]],
  ][type - 1];
  const blocks = [];
  for (const [count, , dataCount] of layout)
    for (let i = 0; i < count; i++) blocks.push({ dataCount, data: [] });

  const maxData = Math.max(...blocks.map((b) => b.dataCount));
  let at = 0;
  for (let i = 0; i < maxData; i++)
    for (const b of blocks) if (i < b.dataCount) b.data.push(bytes[at++]);

  const data = blocks.flatMap((b) => b.data);

  // ---- the byte-mode segment ----
  let bitAt = 0;
  const take = (count) => {
    let v = 0;
    for (let i = 0; i < count; i++) {
      const byte = data[bitAt >> 3];
      const bit = (byte >> (7 - (bitAt & 7))) & 1;
      v = (v << 1) | bit;
      bitAt++;
    }
    return v;
  };
  assert.equal(take(4), 4, "expected 8-bit byte mode");
  const length = take(type < 10 ? 8 : 16);
  const out = [];
  for (let i = 0; i < length; i++) out.push(take(8));
  return new TextDecoder().decode(new Uint8Array(out));
}

describe("qrMatrix", () => {
  it("round-trips every type it claims to support", () => {
    // One payload per type 1..10, sized to land on that type.
    for (const length of [8, 20, 40, 60, 85, 105, 115, 140, 170, 200]) {
      const text = "x".repeat(length);
      const matrix = qrMatrix(text);
      assert.equal(
        decode(matrix),
        text,
        `a ${length}-character payload (type ${(matrix.length - 17) / 4}) did not survive`
      );
    }
  });

  it("round-trips an app pairing URL", () => {
    // The real shape: scheme, host, version, static key, rendezvous secret,
    // LAN hint, pairing code. 165 characters, which is a type 9.
    const url =
      "https://app.ftw.energy/p#v2." +
      "MptMlwkL9s63kKqFva8NwhqUJanQ55Ud-eeVglWavEo." +
      "n0WTkVHk0KHndTLhF9zL3Q." +
      "MTkyLjE2OC4xOTIuNDA6ODA4MA." +
      "Zm9yLXRlc3Rpbmctb25seS1ub3QtYS1yZWFsLWNvZGU";
    const matrix = qrMatrix(url);
    assert.ok((matrix.length - 17) / 4 >= 7, "expected a type 7 or larger");
    assert.equal(decode(matrix), url);
  });

  it("writes version information from type 7 up", () => {
    // The words are fixed by the standard, so a wrong BCH is caught here
    // rather than as a mysterious decode failure.
    const expected = { 7: 0x07c94, 8: 0x085bc, 9: 0x09a99, 10: 0x0a4d3 };
    for (const [type, want] of Object.entries(expected)) {
      const length = { 7: 115, 8: 140, 9: 170, 10: 200 }[type];
      const m = qrMatrix("x".repeat(length));
      assert.equal((m.length - 17) / 4, Number(type), `payload picked another type`);
      const n = m.length;
      let bits = 0;
      for (let i = 0; i < 18; i++)
        if (m[Math.floor(i / 3)][(i % 3) + n - 8 - 3]) bits |= 1 << i;
      assert.equal(bits, want, `type ${type} version info`);

      // And the second copy, which readers fall back to.
      let mirror = 0;
      for (let i = 0; i < 18; i++)
        if (m[(i % 3) + n - 8 - 3][Math.floor(i / 3)]) mirror |= 1 << i;
      assert.equal(mirror, want, `type ${type} version info, second copy`);
    }
  });

  it("keeps the alignment patterns that sit on the timing lines", () => {
    // (6,22) on a type 7. The timing pattern runs through row 6, and drawing
    // it first is what used to swallow this centre.
    const m = qrMatrix("x".repeat(115));
    assert.equal((m.length - 17) / 4, 7);
    // A centre is a dark module surrounded by a light ring.
    assert.equal(m[22][6], true, "centre at (22,6)");
    assert.equal(m[21][5], false, "light ring above-left of (22,6)");
    assert.equal(m[6][22], true, "centre at (6,22)");
    assert.equal(m[5][21], false, "light ring above-left of (6,22)");
  });
});
