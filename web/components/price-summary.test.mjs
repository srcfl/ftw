import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  buildCompactPriceView,
  buildPriceSummary,
  formatPriceSlotLabel,
} from "./price-summary.js";

function at(dayOffset, hour, minute = 0) {
  const date = new Date(2026, 6, 24 + dayOffset, hour, minute, 0, 0);
  return date.getTime();
}

function slot(dayOffset, hour, spot, lenMin = 60) {
  return { tsMs: at(dayOffset, hour), lenMin, spot };
}

describe("compact price summary", () => {
  it("resolves the current slot and VAT using the shared display preference", () => {
    const summary = buildPriceSummary(
      [slot(0, 9, 80), slot(0, 10, 100), slot(0, 11, 60)],
      { now: at(0, 10, 15), vatOn: true, vatPercent: 25 },
    );

    assert.equal(summary.current.tsMs, at(0, 10));
    assert.equal(summary.current.ore, 125);
    assert.equal(summary.today.length, 3);
  });

  it("selects the lowest upcoming slot rather than a past low", () => {
    const summary = buildPriceSummary(
      [
        slot(0, 8, 10),
        slot(0, 10, 90),
        slot(0, 11, 65),
        slot(0, 12, 40),
        slot(1, 1, 30),
      ],
      { now: at(0, 10, 15), vatOn: false, vatPercent: 25 },
    );

    assert.equal(summary.nextLow.tsMs, at(1, 1));
    assert.equal(summary.nextLow.ore, 30);
  });

  it("keeps the profile inside the current local calendar day", () => {
    const summary = buildPriceSummary(
      [slot(-1, 23, 5), slot(0, 0, 50), slot(0, 23, 150), slot(1, 0, 10)],
      { now: at(0, 12), vatOn: false, vatPercent: 25 },
    );

    assert.deepEqual(summary.today.map((item) => item.tsMs), [at(0, 0), at(0, 23)]);
    assert.equal(summary.minOre, 50);
    assert.equal(summary.maxOre, 150);
  });

  it("returns no current slot when wall-clock time falls in a gap", () => {
    const summary = buildPriceSummary(
      [slot(0, 8, 30), slot(0, 12, 20)],
      { now: at(0, 10), vatOn: false, vatPercent: 25 },
    );

    assert.equal(summary.current, null);
    assert.equal(summary.nextLow.tsMs, at(0, 12));
  });

  it("returns a stable empty shape for missing data", () => {
    assert.deepEqual(
      buildPriceSummary([], {
        now: at(0, 10),
        vatOn: true,
        vatPercent: 25,
      }),
      {
        current: null,
        nextLow: null,
        today: [],
        minOre: null,
        maxOre: null,
      },
    );
  });
});

describe("compact price states", () => {
  it("keeps loading, unconfigured, and unavailable states distinct", () => {
    assert.deepEqual(
      buildCompactPriceView({ state: "loading", items: null }),
      { kind: "loading", message: "Loading prices…" },
    );
    assert.deepEqual(
      buildCompactPriceView({ state: "unconfigured", items: null }),
      { kind: "unconfigured", message: "Price unavailable" },
    );
    assert.deepEqual(
      buildCompactPriceView({ state: "error", items: null }),
      { kind: "error", message: "Prices unavailable" },
    );
  });

  it("retains prior data and marks it stale after a later fetch failure", () => {
    const view = buildCompactPriceView({
      state: "stale",
      items: [slot(0, 10, 100), slot(0, 12, 60)],
      now: at(0, 10, 15),
      vatOn: false,
      vatPercent: 25,
    });

    assert.equal(view.kind, "ready");
    assert.equal(view.stale, true);
    assert.equal(view.summary.current.ore, 100);
    assert.equal(view.summary.nextLow.ore, 60);
  });

  it("labels same-day and next-day low windows without a date puzzle", () => {
    assert.equal(formatPriceSlotLabel(at(0, 14), at(0, 10)), "Today 14:00");
    assert.equal(formatPriceSlotLabel(at(1, 1), at(0, 10)), "Tomorrow 01:00");
  });
});
