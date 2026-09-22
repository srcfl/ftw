// Pure helpers for the compact savings card. The endpoint returns one row per
// local day, so the last row tells us the box's date even when the browser is
// in another time zone.

export const SAVINGS_LOOKBACK_DAYS = 31;

export function buildSavingsPeriods(days, savedKey) {
  const key = savedKey || "saved_ore";
  const rows = (Array.isArray(days) ? days : [])
    .filter((row) => row && /^\d{4}-\d{2}-\d{2}$/.test(String(row.day || "")))
    .slice()
    .sort((a, b) => String(a.day).localeCompare(String(b.day)));

  const latest = rows[rows.length - 1];
  const monthKey = latest ? String(latest.day).slice(0, 7) : "";

  return {
    today: summarize(rows.slice(-1), key),
    week: summarize(rows.slice(-7), key),
    month: summarize(monthKey
      ? rows.filter((row) => String(row.day).startsWith(monthKey + "-"))
      : [], key),
  };
}

function summarize(rows, savedKey) {
  const priced = rows.filter((row) => row.resolution !== "no_prices");
  return {
    savedMinor: sum(priced, savedKey),
    pricedDays: priced.length,
    totalDays: rows.length,
    available: priced.length > 0,
    complete: rows.length > 0 && priced.length === rows.length,
  };
}

function sum(rows, key) {
  return rows.reduce((total, row) => total + finite(row[key]), 0);
}

function finite(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number : 0;
}

// Minor units in, signed major units out. The currency sits once in the card
// heading, which leaves enough room for all three values on a phone.
export function formatCompactMinor(minor) {
  const major = finite(minor) / 100;
  const absolute = Math.abs(major);
  const digits = absolute >= 100 ? 0 : (absolute >= 10 ? 1 : 2);
  return (major >= 0 ? "+" : "−") + absolute.toFixed(digits);
}
