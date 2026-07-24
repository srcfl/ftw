export function buildPriceSummary(items, {
  now = Date.now(),
  vatOn = true,
  vatPercent = 25,
} = {}) {
  const vatMultiplier = vatOn ? 1 + (Number(vatPercent) || 0) / 100 : 1;
  const normalized = (Array.isArray(items) ? items : [])
    .map((item) => ({
      tsMs: Number(item && item.tsMs),
      lenMin: Number(item && item.lenMin) || 60,
      ore: (Number(item && item.spot) || 0) * vatMultiplier,
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

  return {
    current,
    nextLow,
    today,
    minOre: todayPrices.length ? Math.min(...todayPrices) : null,
    maxOre: todayPrices.length ? Math.max(...todayPrices) : null,
  };
}

export function buildCompactPriceView({
  state = "loading",
  items = null,
  now = Date.now(),
  vatOn = true,
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
    summary: buildPriceSummary(items, { now, vatOn, vatPercent }),
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
