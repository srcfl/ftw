// Consumer price arithmetic, shared by the price components so no two
// surfaces can drift apart on what a slot costs. Mirrors
// prices.Applier in go/internal/prices — change both together.
//
// Pure functions, no DOM: the tests import this module directly.

// What the slot actually costs to import, in öre/kWh:
// (spot + grid tariff) × (1 + VAT/100).
//
// Leaving the grid tariff out is what made the Overview price card read
// 21 öre for a slot the plan chart priced at 109.
export function consumerTotalOre(spotOre, gridTariffOre, vatPct) {
  return (spotOre + (gridTariffOre || 0)) * (1 + (vatPct || 0) / 100);
}

// The three components of that total, for a breakdown display.
export function priceParts(spotOre, gridTariffOre, vatPct) {
  const grid = gridTariffOre || 0;
  return {
    spot: spotOre,
    grid,
    vat: Math.max(0, (spotOre + grid) * ((vatPct || 0) / 100)),
  };
}

// Cheapest ("min") or dearest ("max") contiguous run of at least `hours`,
// returned as { mean, startMs, endMs } — or null when no run that long
// exists.
//
// Runs are measured by wall-clock duration rather than slot count: a
// window can hold both 15- and 60-minute slots (NordPool moved to
// quarterly PTUs, and stored history straddles the change), so counting
// slots would silently return a 30-minute "2 hour" block. Any gap in the
// series ends a run — a block has to be contiguous to be usable.
//
// `items` must be sorted by tsMs, and `totals[i]` is item `i`'s price.
export function bestBlock(items, totals, hours, mode) {
  const needMs = hours * 3600_000;
  let out = null;
  for (let i = 0; i < items.length; i++) {
    let spanMs = 0, sum = 0, cnt = 0;
    for (let j = i; j < items.length && spanMs < needMs; j++) {
      if (j > i && items[j].tsMs !== items[j - 1].tsMs + items[j - 1].lenMin * 60_000) break;
      spanMs += items[j].lenMin * 60_000;
      sum += totals[j];
      cnt++;
    }
    if (spanMs < needMs || !cnt) continue;
    const mean = sum / cnt;
    if (!out || (mode === "min" ? mean < out.mean : mean > out.mean)) {
      out = { mean, startMs: items[i].tsMs, endMs: items[i].tsMs + spanMs };
    }
  }
  return out;
}
