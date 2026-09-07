// node --test web/components/price-units.test.mjs

import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  activeCurrency,
  formatPrice,
  formatPricePerKwh,
  setActiveCurrency,
  toDisplay,
  unitFor,
  unitLabel,
} from "./price-units.js";

describe("price units", () => {
  it("keeps minor units where households quote them", () => {
    for (const [code, label] of [["SEK", "öre"], ["EUR", "cent"], ["NOK", "øre"], ["DKK", "øre"], ["USD", "¢"], ["GBP", "p"]]) {
      const u = unitFor(code);
      assert.equal(u.label, label);
      assert.equal(u.scale, 1, `${code} should stay in minor units`);
    }
    // 17.4 öre stays 17.4; 17.4 cent stays 17.4.
    assert.equal(formatPrice(17.4, "SEK"), "17.4 öre");
    assert.equal(formatPrice(17.4, "EUR"), "17.4 cent");
  });

  it("shows the major unit where the minor one is out of circulation", () => {
    // 430 haléř is not how anyone quotes power in Czechia — 4.30 Kč is.
    assert.equal(formatPrice(430, "CZK"), "4.30 Kč");
    assert.equal(formatPricePerKwh(4200, "HUF"), "42.0 Ft/kWh");
    assert.equal(formatPrice(85, "RON"), "0.85 lei");
  });

  it("falls back to the ISO code rather than guessing a unit name", () => {
    const u = unitFor("ISK");
    assert.equal(u.label, "ISK");
    assert.equal(u.perKwh, "ISK/kWh");
    assert.equal(u.scale, 0.01);
  });

  it("treats a missing currency as SEK, which is what old installs are in", () => {
    assert.equal(unitLabel(""), "öre");
    assert.equal(unitLabel(undefined), "öre");
    assert.equal(unitLabel("sek"), "öre");
  });

  it("scales values for display without touching the stored number", () => {
    assert.equal(toDisplay(250, "SEK"), 250);
    assert.equal(toDisplay(250, "EUR"), 250);
    assert.equal(Math.round(toDisplay(250, "CZK") * 100) / 100, 2.5);
  });

  it("shares one active currency so every view agrees", () => {
    assert.equal(activeCurrency(), "SEK");
    assert.equal(setActiveCurrency("eur"), "EUR");
    assert.equal(activeCurrency(), "EUR");
    // An empty answer must not wipe a known currency.
    setActiveCurrency("");
    assert.equal(activeCurrency(), "EUR");
    setActiveCurrency("SEK");
  });
});
