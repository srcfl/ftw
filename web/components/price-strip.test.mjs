import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { buildPriceStrip } from "./price-strip.js";

const t0 = Date.UTC(2026, 6, 27, 0, 0, 0);
const slots = (ores, stepMin = 15) =>
  ores.map((ore, i) => ({ tsMs: t0 + i * stepMin * 60_000, lenMin: stepMin, ore }));

describe("compact price strip", () => {
  it("draws each bar from zero, so the strip reads like the full chart", () => {
    // Height is price — the convention every bar chart teaches. The old
    // deviation strip drew the cheapest hours as the tallest bars.
    const strip = buildPriceStrip(slots([100, 200, 300]), { height: 58 });
    const [a, b, c] = strip.bars;
    assert.ok(Math.abs(a.h / c.h - 100 / 300) < 0.01, `heights ${a.h}:${c.h}`);
    assert.ok(Math.abs(b.h / c.h - 200 / 300) < 0.01, `heights ${b.h}:${c.h}`);
    for (const bar of strip.bars) {
      assert.equal((bar.y + bar.h).toFixed(2), strip.zeroY.toFixed(2));
    }
  });

  it("keeps the mean as a reference height, not as the baseline", () => {
    const strip = buildPriceStrip(slots([100, 300]), { height: 100 });
    assert.equal(strip.mean, 200);
    // pad 2, plot 96: zero sits at the bottom, the mean line two thirds up.
    assert.equal(strip.zeroY, 98);
    assert.equal(strip.meanY, 2 + (100 / 300) * 96);
  });

  it("tones each slot by its side of the mean", () => {
    const strip = buildPriceStrip(slots([180, 200, 220, 200]));
    assert.deepEqual(
      strip.bars.map((b) => b.tone),
      ["cheap", "flat", "dear", "flat"],
    );
  });

  it("calls a slot near the mean neither cheap nor dear", () => {
    // mean 200, price span 200, so the flat band is ±10 öre around 200.
    const strip = buildPriceStrip(slots([100, 205, 300, 195]));
    assert.deepEqual(
      strip.bars.map((b) => b.tone),
      ["cheap", "flat", "dear", "flat"],
    );
  });

  it("hangs a negative-price slot below the zero line, toned apart", () => {
    // "They pay you to take it" is its own category, not "cheapest today"
    // — the same reading and colour discipline as the full chart.
    const strip = buildPriceStrip(slots([-50, 100, 150]), { height: 58 });
    const [neg, ...pos] = strip.bars;
    assert.equal(neg.tone, "negative");
    assert.equal(neg.y.toFixed(2), strip.zeroY.toFixed(2));
    assert.ok(neg.h > 1.5, `negative bar too short: ${neg.h}`);
    for (const bar of pos) {
      assert.ok(bar.y + bar.h <= strip.zeroY + 0.01, "positive bar crosses zero");
    }
  });

  it("marks the slot in progress", () => {
    const s = slots([100, 200, 300]);
    const strip = buildPriceStrip(s, { currentTsMs: s[1].tsMs });
    assert.deepEqual(strip.bars.map((b) => b.current), [false, true, false]);
  });

  it("keeps a floor under a bar that would vanish", () => {
    // A near-zero spot slot still draws a sliver, anchored at the baseline.
    const strip = buildPriceStrip(slots([0.5, 300]), { height: 58 });
    const sliver = strip.bars[0];
    assert.equal(sliver.h, 1.5);
    assert.equal((sliver.y + sliver.h).toFixed(2), strip.zeroY.toFixed(2));
  });

  it("draws a nearly flat day as level bars, not amplified noise", () => {
    // The deviation strip stretched a ±1 öre wobble to full height. Drawn
    // from zero, a 150-öre day with a 2-öre spread looks like what it is.
    const strip = buildPriceStrip(slots([150, 149, 151, 150]), { height: 58 });
    const hs = strip.bars.map((b) => b.h);
    const spread = (Math.max(...hs) - Math.min(...hs)) / Math.max(...hs);
    assert.ok(spread < 0.02, `bar heights still spread ${spread}`);
  });

  it("survives an exactly flat day", () => {
    const strip = buildPriceStrip(slots([150, 150, 150, 150]));
    assert.ok(strip.bars.every((b) => b.tone === "flat"));
    assert.ok(strip.bars.every((b) => b.h > 1));
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
});
