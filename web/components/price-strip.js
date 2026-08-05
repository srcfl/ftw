// Geometry for the compact price strip: each slot's price drawn as a bar
// from zero, a miniature of the full chart one tap away.
//
// It used to draw each slot's deviation from the window's mean instead,
// so the fixed grid tariff could not flatten the shape. That traded away
// the one convention every bar chart teaches — height is amount. The
// cheapest morning drew the TALLEST bars (hanging below the line), so the
// strip and the full chart read the same day in opposite directions. Bars
// now rise from zero exactly like the full chart's, and the mean survives
// as a reference line at its own height rather than as the baseline.
//
// Colour never carries the message alone (the theme's green and red sit
// ΔE 2.4 apart under deuteranopia, against a threshold of 8): height is
// the price, the mean line is the cheap/dear reference a bar's top is
// read against, and colour only reinforces that — the strip still works
// in greyscale.
//
// Pure geometry, no DOM: the caller renders and the tests import it.

// Returns null when there is nothing to draw, else
// { zeroY, meanY, mean, bars: [{x, y, w, h, tone, current}], W, H }.
//
// `tone` is "dear"/"cheap" by side of the mean, "flat" within 5 % of the
// price span (a slot that is neither, and shouldn't be pushed onto one
// side of the palette), and "negative" below zero — priced to be taken,
// a different category from "cheapest today", as on the full chart.
export function buildPriceStrip(items, { width = 360, height = 58, currentTsMs = null } = {}) {
  const slots = (Array.isArray(items) ? items : []).filter(
    (it) => it && Number.isFinite(it.ore) && Number.isFinite(it.tsMs),
  );
  if (slots.length < 2) return null;

  const ores = slots.map((it) => it.ore);
  const mean = ores.reduce((a, v) => a + v, 0) / slots.length;
  const hi = Math.max(...ores);
  const lo = Math.min(...ores);
  // Zero stays in view — that is the point of the strip — and a
  // negative-spot slot extends the scale below it, hanging under the
  // baseline. The 1-öre floor keeps an all-zero window drawable.
  const top = Math.max(hi, 1);
  const bottom = Math.min(lo, 0);
  const pad = 2;
  const plotH = height - pad * 2;
  const yFor = (v) => pad + ((top - v) / (top - bottom)) * plotH;
  const zeroY = yFor(0);
  const meanY = yFor(mean);
  const slotW = width / slots.length;
  const flatBand = Math.max(hi - lo, 1) * 0.05;

  const bars = slots.map((it, i) => {
    const h = Math.max(1.5, Math.abs(yFor(it.ore) - zeroY));
    return {
      x: i * slotW + 0.5,
      y: it.ore < 0 ? zeroY : zeroY - h,
      w: Math.max(0.6, slotW - 1),
      h,
      tone: it.ore < 0 ? "negative"
        : Math.abs(it.ore - mean) < flatBand ? "flat"
        : it.ore > mean ? "dear" : "cheap",
      current: currentTsMs != null && it.tsMs === currentTsMs,
    };
  });

  return { zeroY, meanY, mean, bars, W: width, H: height };
}
