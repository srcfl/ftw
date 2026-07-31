// Geometry for the compact price strip: each slot's distance from the
// window's mean, rather than its absolute total.
//
// Why deviation. The consumer total is (spot + grid tariff) × (1 + VAT/100),
// and the tariff is identical every slot — at 70 öre/kWh, ~87 of a 190 öre
// slot never moves. Drawn absolutely, every bar lands within a narrow band
// near the top and the strip carries no information. Measured against the
// mean, the fixed floor drops out and the shape of the day is what is left.
//
// Why direction rather than colour. The theme's green and red measure ΔE 2.4
// apart under deuteranopia in light mode, against a threshold of 8 — a strip
// that encodes cheap-vs-dear in hue alone is unreadable for roughly one man
// in twelve. Above the line is dearer, below is cheaper; colour only
// reinforces what the geometry already says, so it survives greyscale.
//
// Pure geometry, no DOM: the caller renders and the tests import it.

// Returns null when there is nothing to draw, else
// { midY, bars: [{x, y, w, h, side, current}], mean, W, H }.
//
// `side` is "up" for dearer than mean, "down" for cheaper, "flat" for within
// 5 % of the span — a slot that is neither, and shouldn't be pushed onto one
// side of the palette.
export function buildPriceStrip(items, { width = 360, height = 58, currentTsMs = null } = {}) {
  const slots = (Array.isArray(items) ? items : []).filter(
    (it) => it && Number.isFinite(it.ore) && Number.isFinite(it.tsMs),
  );
  if (slots.length < 2) return null;

  const mean = slots.reduce((a, it) => a + it.ore, 0) / slots.length;
  const devs = slots.map((it) => it.ore - mean);
  const up = Math.max(...devs, 0);
  const down = Math.max(...devs.map((d) => -d), 0);
  // One span for both arms, so an equal saving and surcharge are equally
  // tall. The line sits by the ratio of the extremes rather than at
  // mid-height, so neither arm is squashed into a stub on a lopsided day.
  const span = Math.max(up + down, 1);
  const pad = 2;
  const plotH = height - pad * 2;
  const midY = pad + (up / span) * plotH;
  const slotW = width / slots.length;

  const bars = slots.map((it, i) => {
    const d = devs[i];
    const h = Math.max(1.5, (Math.abs(d) / span) * plotH);
    return {
      x: i * slotW + 0.5,
      y: d >= 0 ? midY - h : midY,
      w: Math.max(0.6, slotW - 1),
      h,
      side: Math.abs(d) < span * 0.05 ? "flat" : d >= 0 ? "up" : "down",
      current: currentTsMs != null && it.tsMs === currentTsMs,
    };
  });

  return { midY, bars, mean, W: width, H: height };
}
