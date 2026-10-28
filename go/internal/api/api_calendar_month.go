package api

import (
	"fmt"
	"time"
)

// calendarMonthBounds returns local-midnight boundaries for a selectable month.
// A future month is not a history range; the current month remains partial in
// the daily handler, which clips each day's end to now.
func calendarMonthBounds(value string, now time.Time) (time.Time, time.Time, error) {
	month, err := time.ParseInLocation("2006-01", value, now.Location())
	if err != nil || month.Format("2006-01") != value {
		return time.Time{}, time.Time{}, fmt.Errorf("month must be YYYY-MM")
	}
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, now.Location())
	if start.After(now) {
		return time.Time{}, time.Time{}, fmt.Errorf("month cannot be in the future")
	}
	return start, start.AddDate(0, 1, 0), nil
}
