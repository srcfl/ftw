import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  attributionNote,
  costFootnote,
  formatEVCost,
  formatEnergyKWh,
  windowById,
} from "./ev-energy-format.js";

describe("EV energy display", () => {
  it("formats energy and currency from minor units", () => {
    assert.equal(formatEnergyKWh(42300), "42.3 kWh");
    assert.equal(formatEnergyKWh(180100), "180 kWh");
    assert.equal(formatEVCost(18640, 0, "EUR"), "186 EUR");
    assert.equal(formatEVCost(864, 0, "EUR"), "8.64 EUR");
    assert.equal(formatEVCost(18640, 500, "SEK"), "186 SEK (partial)");
    assert.equal(formatEVCost(0, 1000, "SEK"), "No price");
    assert.equal(formatEVCost(0, 0, "SEK"), "0.00 SEK");
  });

  it("explains a price gap without hiding the grid-share rule", () => {
    const note = costFootnote({ attribution: "site_mix", cost_partial: true });
    assert.match(note, /grid share/);
    assert.match(note, /no electricity price/);
    assert.match(attributionNote("import_price"), /priced as grid import/);
    assert.equal(windowById([{ id: "7d" }, { id: "30d", energy_wh: 3 }], "30d").energy_wh, 3);
  });
});
