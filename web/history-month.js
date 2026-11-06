// Calendar month helpers for periods selected in the box's runtime timezone.

export function isCalendarMonthKey(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}$/.test(value)) return false;
  const year = Number(value.slice(0, 4));
  const month = Number(value.slice(5, 7));
  return year >= 1 && month >= 1 && month <= 12;
}

export function monthKeyAt(date, timeZone) {
  if (!(date instanceof Date) || Number.isNaN(date.getTime())) return null;
  if (typeof timeZone !== "string" || !timeZone.trim()) return null;

  try {
    const parts = new Intl.DateTimeFormat("en", {
      calendar: "gregory",
      numberingSystem: "latn",
      timeZone,
      year: "numeric",
      month: "2-digit",
    }).formatToParts(date);
    const year = parts.find((part) => part.type === "year")?.value;
    const month = parts.find((part) => part.type === "month")?.value;
    const key = `${year}-${month}`;
    return isCalendarMonthKey(key) ? key : null;
  } catch {
    return null;
  }
}

export function shiftMonthKey(monthKey, offset) {
  if (!isCalendarMonthKey(monthKey) || !Number.isInteger(offset)) return null;
  const index = Number(monthKey.slice(0, 4)) * 12 + Number(monthKey.slice(5, 7)) - 1 + offset;
  const year = Math.floor(index / 12);
  if (year < 1 || year > 9999) return null;
  return `${String(year).padStart(4, "0")}-${String(index % 12 + 1).padStart(2, "0")}`;
}

export function runtimeTimezoneFromSchema(schema) {
  const timeZone = schema && schema.runtime_timezone;
  if (typeof timeZone !== "string" || !monthKeyAt(new Date(0), timeZone)) {
    throw new Error("The box returned an invalid runtime timezone");
  }
  return timeZone;
}
