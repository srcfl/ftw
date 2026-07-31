import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { buildPriceStrip } from "./price-strip.js";

const t0 = Date.UTC(2026, 6, 27, 0, 0, 0);
const slots = (ores, stepMin = 15) =>
  ores.map((ore, i) => ({ tsMs: t0 + i * stepMin * 60_000, lenMin: stepMin, ore }));

describe("compact price strip", () => {
  it("measures each slot against the mean, not against zero", () => {
    // The whole point: a fixed grid tariff makes absolute totals nearly
    // equal, so bars drawn from zero carry no information. Here every slot
    // sits 100 öre up on a flat floor and only the spread should show.
    const strip = buildPriceStrip(slots([180, 200, 220, 200]));
    assert.equal(strip.mean, 200);
    const sides = strip.bars.map((b) => b.side);
    assert.deepEqual(sides, ["down", "flat", "up", "flat"]);
  });

  it("gives an öre the same height above and below the line", () => {
    const strip = buildPriceStrip(slots([100, 300]), { height: 100 });
    const [low, high] = strip.bars;
    assert.equal(low.h.toFixed(2), high.h.toFixed(2));
  });

  it("places the line by the extremes so neither arm is a stub", () => {
    // 100 over, 20 under: the line sits high, and the cheap arm still gets
    // real height instead of being squashed against a centred axis.
    const strip = buildPriceStrip(slots([80, 100, 200]), { height: 100 });
    const cheap = strip.bars.find((b) => b.side === "down");
    assert.ok(cheap.h > 4, `cheap arm too short: ${cheap.h}`);
    assert.ok(strip.midY > 50, `line should sit below centre, got ${strip.midY}`);
  });

  it("marks the slot in progress", () => {
    const s = slots([100, 200, 300]);
    const strip = buildPriceStrip(s, { currentTsMs: s[1].tsMs });
    assert.deepEqual(strip.bars.map((b) => b.current), [false, true, false]);
  });

  it("calls a slot near the mean neither cheap nor dear", () => {
    // mean 200, span 200, so the flat band is ±10 öre around 200.
    const strip = buildPriceStrip(slots([100, 205, 300, 195]));
    assert.deepEqual(strip.bars.map((b) => b.side), ["down", "flat", "up", "flat"]);
  });

  it("returns null rather than a degenerate strip", () => {
    assert.equal(buildPriceStrip([]), null);
    assert.equal(buildPriceStrip([{ tsMs: t0, lenMin: 15, ore: 100 }]), null);
    assert.equal(buildPriceStrip(null), null);
  });

  it("ignores slots with unusable numbers", () => {
    const strip = buildPriceStrip([
      { tsMs: t0, lenMin: 15, ore: 100 },
      { tsMs: NaN, lenMin: 15, ore: 200 },
      { tsMs: t0 + 900_000, lenMin: 15, ore: 300 },
    ]);
    assert.equal(strip.bars.length, 2);
  });

  it("never draws a zero-height bar on a flat day", () => {
    const strip = buildPriceStrip(slots([150, 150, 150, 150]));
    for (const b of strip.bars) assert.ok(b.h >= 1.5, `bar height ${b.h}`);
  });
});
