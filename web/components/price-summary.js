import { consumerTotalOre } from "./price-math.js";

// Re-exported so the compact card can search for a usable window without
// importing two price modules.
export { bestBlock } from "./price-math.js";

// `totalOn` picks the consumer total — (spot + grid tariff) × (1 + VAT/100),
// the same arithmetic as prices.Applier and the plan chart — over raw spot.
// Applying VAT to bare spot here reported 21 öre for a slot that costs 109
// on a 70 öre/kWh tariff, which is the number this summary leads with.
export function buildPriceSummary(items, {
  now = Date.now(),
  totalOn = true,
  gridTariffOre = 0,
  vatPercent = 25,
} = {}) {
  const resolve = (spot) => (totalOn
    ? consumerTotalOre(spot, gridTariffOre, vatPercent)
    : spot);
  const normalized = (Array.isArray(items) ? items : [])
    .map((item) => ({
      tsMs: Number(item && item.tsMs),
      lenMin: Number(item && item.lenMin) || 60,
      ore: resolve(Number(item && item.spot) || 0),
    }))
    .filter((item) => Number.isFinite(item.tsMs))
    .sort((a, b) => a.tsMs - b.tsMs);

  const current = normalized.find((item) => (
    now >= item.tsMs && now < item.tsMs + item.lenMin * 60_000
  )) || null;

  const nextLow = normalized
    .filter((item) => item.tsMs > now)
    .reduce((lowest, item) => (
      !lowest || item.ore < lowest.ore ? item : lowest
    ), null);

  const dayStart = new Date(now);
  dayStart.setHours(0, 0, 0, 0);
  const dayEnd = new Date(dayStart);
  dayEnd.setDate(dayEnd.getDate() + 1);
  const today = normalized.filter((item) => (
    item.tsMs >= dayStart.getTime() && item.tsMs < dayEnd.getTime()
  ));
  const todayPrices = today.map((item) => item.ore);

  // Everything still ahead, including the slot in progress — what a window
  // search has to work from. `today` stops at midnight, so on an evening it
  // holds too little to find a usable block in.
  const upcoming = normalized.filter(
    (item) => item.tsMs + item.lenMin * 60_000 > now,
  );

  return {
    current,
    nextLow,
    upcoming,
    today,
    minOre: todayPrices.length ? Math.min(...todayPrices) : null,
    maxOre: todayPrices.length ? Math.max(...todayPrices) : null,
  };
}

export function buildCompactPriceView({
  state = "loading",
  items = null,
  now = Date.now(),
  totalOn = true,
  gridTariffOre = 0,
  vatPercent = 25,
} = {}) {
  if (!Array.isArray(items)) {
    if (state === "unconfigured") {
      return { kind: "unconfigured", message: "Price unavailable" };
    }
    if (state === "error") {
      return { kind: "error", message: "Prices unavailable" };
    }
    return { kind: "loading", message: "Loading prices…" };
  }
  if (items.length === 0) {
    return { kind: "empty", message: "No prices published for today." };
  }
  return {
    kind: "ready",
    stale: state === "stale",
    summary: buildPriceSummary(items, { now, totalOn, gridTariffOre, vatPercent }),
  };
}

export function formatPriceSlotLabel(tsMs, now = Date.now()) {
  const slot = new Date(tsMs);
  const today = new Date(now);
  const tomorrow = new Date(now);
  tomorrow.setDate(tomorrow.getDate() + 1);
  const dayKey = (date) => (
    `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`
  );
  const prefix = dayKey(slot) === dayKey(today)
    ? "Today"
    : dayKey(slot) === dayKey(tomorrow)
      ? "Tomorrow"
      : slot.toLocaleDateString(undefined, { weekday: "short" });
  const clock = `${String(slot.getHours()).padStart(2, "0")}:${String(slot.getMinutes()).padStart(2, "0")}`;
  return `${prefix} ${clock}`;
}
