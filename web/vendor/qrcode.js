// qrcode.js — zero-dependency QR Code (Model 2) encoder, byte mode.
//
// SOURCE: Kazuhiko Arase's "qrcode-generator"
// (https://github.com/kazuhikoarase/qrcode-generator).
// LICENSE: MIT.
//   Copyright (c) 2009 Kazuhiko Arase
//   Permission is hereby granted, free of charge, to any person obtaining a copy
//   of this software and associated documentation files (the "Software"), to deal
//   in the Software without restriction, including without limitation the rights
//   to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//   copies of the Software, and to permit persons to whom the Software is
//   furnished to do so, subject to the conditions of the MIT License.
//
// This is a faithful, trimmed ES-module port of the upstream algorithm: 8-bit
// byte mode only (sufficient for a URL), Reed-Solomon error correction via the
// GF(256) tables below, the eight mask patterns + penalty scoring, and automatic
// type (version) selection for the chosen error-correction level. We DO NOT
// hand-roll the Reed-Solomon — the polynomial arithmetic and the per-type RS
// block layout are the upstream tables, reproduced verbatim. We dropped the
// canvas/img rendering, numeric/alphanumeric/kanji modes, and the global
// registration the upstream ships, exposing instead a single pure function that
// returns a boolean module matrix the caller can paint however it likes.
//
// Vendored (not via CDN / npm) so a fresh Pi boots and renders the onboarding QR
// fully offline, per DESIGN.md ("No network fonts" / fresh-Pi-without-WAN rule).

// ---- GF(256) math (upstream QRMath) ----
const EXP = new Array(256);
const LOG = new Array(256);
for (let i = 0; i < 8; i++) EXP[i] = 1 << i;
for (let i = 8; i < 256; i++) {
  EXP[i] = EXP[i - 4] ^ EXP[i - 5] ^ EXP[i - 6] ^ EXP[i - 8];
}
for (let i = 0; i < 255; i++) LOG[EXP[i]] = i;
function gexp(n) {
  while (n < 0) n += 255;
  while (n >= 256) n -= 255;
  return EXP[n];
}
function glog(n) {
  if (n < 1) throw new Error("glog(" + n + ")");
  return LOG[n];
}

// ---- polynomial (upstream QRPolynomial) ----
function Polynomial(num, shift) {
  let offset = 0;
  while (offset < num.length && num[offset] === 0) offset++;
  const n = new Array(num.length - offset + shift);
  for (let i = 0; i < num.length - offset; i++) n[i] = num[i + offset];
  return n;
}
function polyMultiply(a, b) {
  const n = new Array(a.length + b.length - 1).fill(0);
  for (let i = 0; i < a.length; i++) {
    for (let j = 0; j < b.length; j++) {
      n[i + j] ^= gexp(glog(a[i]) + glog(b[j]));
    }
  }
  return Polynomial(n, 0);
}
function polyMod(self, e) {
  if (self.length - e.length < 0) return self;
  const ratio = glog(self[0]) - glog(e[0]);
  const n = self.slice();
  for (let i = 0; i < e.length; i++) n[i] ^= gexp(glog(e[i]) + ratio);
  return polyMod(Polynomial(n, 0), e);
}
function rsBlockPoly(errorCount) {
  let a = [1];
  for (let i = 0; i < errorCount; i++) a = polyMultiply(a, [1, gexp(i)]);
  return a;
}

// ---- BitBuffer (upstream QRBitBuffer) ----
function BitBuffer() {
  return { buffer: [], length: 0 };
}
function putBit(buf, bit) {
  const idx = Math.floor(buf.length / 8);
  if (buf.buffer.length <= idx) buf.buffer.push(0);
  if (bit) buf.buffer[idx] |= 0x80 >>> buf.length % 8;
  buf.length++;
}
function put(buf, num, len) {
  for (let i = 0; i < len; i++) putBit(buf, ((num >>> (len - i - 1)) & 1) === 1);
}

// ---- RS block layout (upstream QRRSBlock.RS_BLOCK_TABLE), level M only ----
// Each row: [total-blocks descriptor] flattened as [count, totalCount, dataCount]
// repeated. We carry only error-correction level M (the standard mid level), one
// row per type (version) 1..10 — ample for a ~80-char onboarding URL.
const RS_BLOCK_M = [
  /* type 1  */[[1, 26, 16]],
  /* type 2  */[[1, 44, 28]],
  /* type 3  */[[1, 70, 44]],
  /* type 4  */[[2, 50, 32]],
  /* type 5  */[[2, 67, 43]],
  /* type 6  */[[4, 43, 27]],
  /* type 7  */[[4, 49, 31]],
  /* type 8  */[[2, 60, 38], [2, 61, 39]],
  /* type 9  */[[3, 58, 36], [2, 59, 37]],
  /* type 10 */[[4, 69, 43], [1, 70, 44]],
];
const EC_LEVEL_M = 0; // QRErrorCorrectLevel.M
const MODE_8BIT = 4; // QRMode.MODE_8BIT_BYTE

function rsBlocks(typeNumber) {
  const row = RS_BLOCK_M[typeNumber - 1];
  const list = [];
  for (const [count, totalCount, dataCount] of row) {
    for (let i = 0; i < count; i++) list.push({ totalCount, dataCount });
  }
  return list;
}

// ---- BCH (format + version info), upstream QRUtil ----
const G15 =
  (1 << 10) | (1 << 8) | (1 << 5) | (1 << 4) | (1 << 2) | (1 << 1) | (1 << 0);
const G15_MASK = (1 << 14) | (1 << 12) | (1 << 10) | (1 << 4) | (1 << 1);
function bchDigit(data) {
  let digit = 0;
  while (data !== 0) { digit++; data >>>= 1; }
  return digit;
}
function bchTypeInfo(data) {
  let d = data << 10;
  while (bchDigit(d) - bchDigit(G15) >= 0) d ^= G15 << (bchDigit(d) - bchDigit(G15));
  return ((data << 10) | d) ^ G15_MASK;
}

// ---- alignment-pattern centres (upstream QRUtil.PATTERN_POSITION_TABLE) ----
const ALIGN_POS = [
  [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34],
  [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50],
];

// ---- mask functions (upstream QRMaskPattern) ----
function maskFn(maskPattern, i, j) {
  switch (maskPattern) {
    case 0: return (i + j) % 2 === 0;
    case 1: return i % 2 === 0;
    case 2: return j % 3 === 0;
    case 3: return (i + j) % 3 === 0;
    case 4: return (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0;
    case 5: return ((i * j) % 2) + ((i * j) % 3) === 0;
    case 6: return (((i * j) % 2) + ((i * j) % 3)) % 2 === 0;
    case 7: return (((i * j) % 3) + ((i + j) % 2)) % 2 === 0;
    default: throw new Error("bad maskPattern:" + maskPattern);
  }
}

// ---- QR model (upstream QRCodeModel), trimmed to byte mode ----
function createData(typeNumber, dataBytes) {
  const blocks = rsBlocks(typeNumber);
  const buf = BitBuffer();
  put(buf, MODE_8BIT, 4);
  // byte-mode length is 8 bits for type 1..9, 16 bits for 10..26.
  put(buf, dataBytes.length, typeNumber < 10 ? 8 : 16);
  for (const b of dataBytes) put(buf, b, 8);

  let totalDataCount = 0;
  for (const blk of blocks) totalDataCount += blk.dataCount;
  if (buf.length > totalDataCount * 8) {
    throw new Error("data length overflow (" + buf.length + " > " + totalDataCount * 8 + ")");
  }
  if (buf.length + 4 <= totalDataCount * 8) put(buf, 0, 4); // terminator
  while (buf.length % 8 !== 0) putBit(buf, false);
  // pad with the alternating bytes 0xEC, 0x11 (upstream PAD0 / PAD1).
  while (true) {
    if (buf.buffer.length >= totalDataCount) break;
    buf.buffer.push(0xec);
    if (buf.buffer.length >= totalDataCount) break;
    buf.buffer.push(0x11);
  }
  return createBytes(buf, blocks);
}

function createBytes(buffer, rsBlockList) {
  let offset = 0;
  let maxDc = 0, maxEc = 0;
  const dcdata = new Array(rsBlockList.length);
  const ecdata = new Array(rsBlockList.length);
  for (let r = 0; r < rsBlockList.length; r++) {
    const dcCount = rsBlockList[r].dataCount;
    const ecCount = rsBlockList[r].totalCount - dcCount;
    maxDc = Math.max(maxDc, dcCount);
    maxEc = Math.max(maxEc, ecCount);
    dcdata[r] = new Array(dcCount);
    for (let i = 0; i < dcCount; i++) dcdata[r][i] = 0xff & buffer.buffer[i + offset];
    offset += dcCount;
    const rsPoly = rsBlockPoly(ecCount);
    const rawPoly = Polynomial(dcdata[r], rsPoly.length - 1);
    const modPoly = polyMod(rawPoly, rsPoly);
    ecdata[r] = new Array(rsPoly.length - 1);
    for (let i = 0; i < ecdata[r].length; i++) {
      const modIndex = i + modPoly.length - ecdata[r].length;
      ecdata[r][i] = modIndex >= 0 ? modPoly[modIndex] : 0;
    }
  }
  let totalCodeCount = 0;
  for (const blk of rsBlockList) totalCodeCount += blk.totalCount;
  const data = new Array(totalCodeCount);
  let index = 0;
  for (let i = 0; i < maxDc; i++) {
    for (let r = 0; r < rsBlockList.length; r++) {
      if (i < dcdata[r].length) data[index++] = dcdata[r][i];
    }
  }
  for (let i = 0; i < maxEc; i++) {
    for (let r = 0; r < rsBlockList.length; r++) {
      if (i < ecdata[r].length) data[index++] = ecdata[r][i];
    }
  }
  return data;
}

// makeModules builds the module matrix for a given type + mask, including the
// finder/alignment/timing/format patterns. Upstream QRCodeModel.makeImpl.
function makeModules(typeNumber, data, maskPattern) {
  const moduleCount = typeNumber * 4 + 17;
  const modules = [];
  for (let r = 0; r < moduleCount; r++) modules.push(new Array(moduleCount).fill(null));

  function setupPositionProbe(row, col) {
    for (let r = -1; r <= 7; r++) {
      if (row + r <= -1 || moduleCount <= row + r) continue;
      for (let c = -1; c <= 7; c++) {
        if (col + c <= -1 || moduleCount <= col + c) continue;
        modules[row + r][col + c] =
          (r >= 0 && r <= 6 && (c === 0 || c === 6)) ||
          (c >= 0 && c <= 6 && (r === 0 || r === 6)) ||
          (r >= 2 && r <= 4 && c >= 2 && c <= 4);
      }
    }
  }
  setupPositionProbe(0, 0);
  setupPositionProbe(moduleCount - 7, 0);
  setupPositionProbe(0, moduleCount - 7);

  // timing patterns
  for (let r = 8; r < moduleCount - 8; r++) {
    if (modules[r][6] === null) modules[r][6] = r % 2 === 0;
  }
  for (let c = 8; c < moduleCount - 8; c++) {
    if (modules[6][c] === null) modules[6][c] = c % 2 === 0;
  }

  // alignment patterns
  const pos = ALIGN_POS[typeNumber - 1];
  for (let i = 0; i < pos.length; i++) {
    for (let j = 0; j < pos.length; j++) {
      const row = pos[i], col = pos[j];
      if (modules[row][col] !== null) continue;
      for (let r = -2; r <= 2; r++) {
        for (let c = -2; c <= 2; c++) {
          modules[row + r][col + c] =
            r === -2 || r === 2 || c === -2 || c === 2 || (r === 0 && c === 0);
        }
      }
    }
  }

  // format info (EC level M + mask), placed twice.
  const formatBits = bchTypeInfo((EC_LEVEL_M << 3) | maskPattern);
  for (let i = 0; i < 15; i++) {
    const mod = ((formatBits >> i) & 1) === 1;
    if (i < 6) modules[i][8] = mod;
    else if (i < 8) modules[i + 1][8] = mod;
    else modules[moduleCount - 15 + i][8] = mod;
  }
  for (let i = 0; i < 15; i++) {
    const mod = ((formatBits >> i) & 1) === 1;
    if (i < 8) modules[8][moduleCount - i - 1] = mod;
    else if (i < 9) modules[8][15 - i - 1 + 1] = mod;
    else modules[8][15 - i - 1] = mod;
  }
  modules[moduleCount - 8][8] = true; // dark module

  // data placement with mask
  let inc = -1, row = moduleCount - 1, bitIndex = 7, byteIndex = 0;
  for (let col = moduleCount - 1; col > 0; col -= 2) {
    if (col === 6) col--;
    while (true) {
      for (let c = 0; c < 2; c++) {
        if (modules[row][col - c] === null) {
          let dark = false;
          if (byteIndex < data.length) {
            dark = ((data[byteIndex] >>> bitIndex) & 1) === 1;
          }
          if (maskFn(maskPattern, row, col - c)) dark = !dark;
          modules[row][col - c] = dark;
          bitIndex--;
          if (bitIndex === -1) { byteIndex++; bitIndex = 7; }
        }
      }
      row += inc;
      if (row < 0 || moduleCount <= row) { row -= inc; inc = -inc; break; }
    }
  }
  return modules;
}

// ---- mask penalty scoring (upstream QRUtil.getLostPoint) ----
function lostPoint(modules) {
  const n = modules.length;
  let lost = 0;
  // rule 1: 5+ same-colour in a row/col
  for (let row = 0; row < n; row++) {
    for (let col = 0; col < n; col++) {
      let same = 0;
      const dark = modules[row][col];
      for (let r = -1; r <= 1; r++) {
        if (row + r < 0 || n <= row + r) continue;
        for (let c = -1; c <= 1; c++) {
          if (col + c < 0 || n <= col + c) continue;
          if (r === 0 && c === 0) continue;
          if (dark === modules[row + r][col + c]) same++;
        }
      }
      if (same > 5) lost += 3 + same - 5;
    }
  }
  // rule 2: 2x2 blocks
  for (let row = 0; row < n - 1; row++) {
    for (let col = 0; col < n - 1; col++) {
      let count = 0;
      if (modules[row][col]) count++;
      if (modules[row + 1][col]) count++;
      if (modules[row][col + 1]) count++;
      if (modules[row + 1][col + 1]) count++;
      if (count === 0 || count === 4) lost += 3;
    }
  }
  // rule 3: 1:1:3:1:1 finder-like patterns
  for (let row = 0; row < n; row++) {
    for (let col = 0; col < n - 6; col++) {
      if (
        modules[row][col] && !modules[row][col + 1] && modules[row][col + 2] &&
        modules[row][col + 3] && modules[row][col + 4] && !modules[row][col + 5] &&
        modules[row][col + 6]
      ) lost += 40;
    }
  }
  for (let col = 0; col < n; col++) {
    for (let row = 0; row < n - 6; row++) {
      if (
        modules[row][col] && !modules[row + 1][col] && modules[row + 2][col] &&
        modules[row + 3][col] && modules[row + 4][col] && !modules[row + 5][col] &&
        modules[row + 6][col]
      ) lost += 40;
    }
  }
  // rule 4: dark-module balance
  let darkCount = 0;
  for (let row = 0; row < n; row++) {
    for (let col = 0; col < n; col++) if (modules[row][col]) darkCount++;
  }
  const ratio = Math.abs((100 * darkCount) / (n * n) - 50) / 5;
  lost += ratio * 10;
  return lost;
}

function bestMaskModules(typeNumber, data) {
  let best = null, bestScore = Infinity;
  for (let mask = 0; mask < 8; mask++) {
    const m = makeModules(typeNumber, data, mask);
    const score = lostPoint(m);
    if (score < bestScore) { bestScore = score; best = m; }
  }
  return best;
}

// ---- public API ----

// utf8Bytes encodes a string to a UTF-8 byte array (the QR byte-mode payload).
function utf8Bytes(str) {
  const out = [];
  for (let i = 0; i < str.length; i++) {
    let c = str.charCodeAt(i);
    if (c < 0x80) out.push(c);
    else if (c < 0x800) {
      out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f));
    } else if (c < 0xd800 || c >= 0xe000) {
      out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
    } else {
      // surrogate pair
      i++;
      c = 0x10000 + (((c & 0x3ff) << 10) | (str.charCodeAt(i) & 0x3ff));
      out.push(
        0xf0 | (c >> 18), 0x80 | ((c >> 12) & 0x3f),
        0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f),
      );
    }
  }
  return out;
}

// chooseType picks the smallest type (version, 1..10) whose level-M data
// capacity holds the byte payload. Throws if the payload exceeds type 10
// (~271 data bytes at level M — far beyond any onboarding URL).
function chooseType(byteLen) {
  for (let t = 1; t <= RS_BLOCK_M.length; t++) {
    let dataCount = 0;
    for (const blk of rsBlocks(t)) dataCount += blk.dataCount;
    // mode (4) + length (8 or 16) + 8 bits per byte + 4-bit terminator, in bytes.
    const headerBits = 4 + (t < 10 ? 8 : 16);
    const needBits = headerBits + byteLen * 8;
    if (needBits <= dataCount * 8) return t;
  }
  throw new Error("payload too large for QR type<=10");
}

// qrMatrix encodes `text` (UTF-8 byte mode, EC level M, best mask auto-selected)
// and returns a square boolean matrix: matrix[row][col] === true means a DARK
// module. The caller paints it (canvas, divs, whatever). Deterministic for a
// fixed input. No quiet zone is included — add ≥4 modules of margin when drawing.
export function qrMatrix(text) {
  const bytes = utf8Bytes(String(text));
  const typeNumber = chooseType(bytes.length);
  const data = createData(typeNumber, bytes);
  const modules = bestMaskModules(typeNumber, data);
  // Normalise nulls (should not remain after data placement) to false.
  return modules.map((row) => row.map((v) => v === true));
}

if (typeof window !== "undefined") {
  window.ftwQR = { qrMatrix };
}
