import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { bestBlock, consumerTotalOre, priceParts } from "./components/price-math.js";

// The slot Fredrik hovered on 2026-07-27: spot 17.4 öre, 70 öre grid
// tariff, 25 % VAT. The plan chart said 109.3; the Overview price card
// said 21.2 because it left the tariff out.
describe("consumer price", () => {
  it("charges spot plus grid tariff plus VAT on both", () => {
    assert.equal(consumerTotalOre(17.4, 70, 25).toFixed(1), "109.3");
  });

  it("is spot plus VAT when there is no grid tariff", () => {
    assert.equal(consumerTotalOre(17.4, 0, 25).toFixed(2), "21.75");
  });

  it("keeps negative spot negative rather than clamping it", () => {
    // A negative-spot hour still owes the tariff, so the total can land
    // either side of zero — the arithmetic must not floor it.
    assert.ok(consumerTotalOre(-100, 70, 25) < 0);
    assert.equal(consumerTotalOre(-20, 70, 25).toFixed(1), "62.5");
  });

  it("treats missing tariff and VAT as zero, not NaN", () => {
    assert.equal(consumerTotalOre(50, undefined, undefined), 50);
  });

  it("breaks the total into parts that sum back to it", () => {
    const p = priceParts(17.4, 70, 25);
    assert.equal(p.spot, 17.4);
    assert.equal(p.grid, 70);
    assert.equal(p.vat.toFixed(1), "21.9");
    assert.equal((p.spot + p.grid + p.vat).toFixed(1), consumerTotalOre(17.4, 70, 25).toFixed(1));
  });
});

// Helper: n quarter-hour slots starting at t0.
function quarters(t0, prices) {
  return prices.map((_, i) => ({ tsMs: t0 + i * 15 * 60_000, lenMin: 15 }));
}

describe("cheapest / dearest block", () => {
  const t0 = Date.UTC(2026, 6, 27, 0, 0, 0);

  it("finds the cheapest contiguous two hours, not the cheapest slot", () => {
    // Slot 0 is the single cheapest, but the cheapest 2 h run (8 slots)
    // is the flat stretch that follows it.
    const totals = [1, 200, 200, 200, 200, 200, 200, 200, 10, 10, 10, 10, 10, 10, 10, 10];
    const items = quarters(t0, totals);
    const b = bestBlock(items, totals, 2, "min");
    assert.equal(b.startMs, t0 + 8 * 15 * 60_000);
    assert.equal(b.endMs, b.startMs + 2 * 3600_000);
    assert.equal(b.mean, 10);
  });

  it("finds the dearest contiguous two hours", () => {
    const totals = [10, 10, 10, 10, 300, 300, 300, 300, 300, 300, 300, 300];
    const items = quarters(t0, totals);
    const b = bestBlock(items, totals, 2, "max");
    assert.equal(b.startMs, t0 + 4 * 15 * 60_000);
    assert.equal(b.mean, 300);
  });

  it("measures the block in wall-clock time, not slot count", () => {
    // Two 60-minute slots are a valid 2 h block; eight would be 8 h.
    const items = [
      { tsMs: t0, lenMin: 60 },
      { tsMs: t0 + 3600_000, lenMin: 60 },
    ];
    const b = bestBlock(items, [50, 70], 2, "min");
    assert.equal(b.startMs, t0);
    assert.equal(b.endMs, t0 + 2 * 3600_000);
    assert.equal(b.mean, 60);
  });

  it("refuses to bridge a gap in the series", () => {
    // 1 h of slots, an hour missing, then 1 h more — no contiguous 2 h
    // block exists even though there are 8 slots.
    const items = [...quarters(t0, [0, 0, 0, 0]), ...quarters(t0 + 2 * 3600_000, [0, 0, 0, 0])];
    assert.equal(bestBlock(items, items.map(() => 50), 2, "min"), null);
  });

  it("returns null when the window is shorter than one block", () => {
    const totals = [10, 20, 30, 40];
    assert.equal(bestBlock(quarters(t0, totals), totals, 2, "min"), null);
  });

  it("returns null on empty input", () => {
    assert.equal(bestBlock([], [], 2, "min"), null);
  });
});
