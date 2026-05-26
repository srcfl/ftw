package loadpoint

import "time"

// Schedule is the user's persistent charging intent for one loadpoint:
// "be at SoCPct by TimeOfDayMinUTC each day". When Recurring is true the
// Manager rolls the loadpoint's targetTime forward to tomorrow once
// today's deadline passes; when false the schedule still hydrates the
// one-shot target_soc_pct/target_time fields on save but doesn't refresh
// itself.
//
// SurplusUnlockBatSoCPct, if > 0, tells the dispatch controller to grab
// PV surplus into this loadpoint whenever the home battery's SoC sits at
// or above the threshold — even when SurplusOnly is off and the MPC has
// nothing planned. Hysteresis (release at threshold − BatSoCUnlockHystPp)
// keeps the contactor from flapping at the boundary.
//
// Zero value (Empty) means "no schedule configured". Persistence keys
// off this — Empty schedules are not written to disk.
type Schedule struct {
	SoCPct                 float64 `json:"soc_pct"`
	TimeOfDayMinUTC        int     `json:"time_of_day_min_utc"` // 0..1439
	Recurring              bool    `json:"recurring"`
	SurplusUnlockBatSoCPct float64 `json:"surplus_unlock_bat_soc_pct,omitempty"`
}

// BatSoCUnlockHystPp is the percentage-point gap between arm and release
// for the bat-SoC surplus unlock. Armed at threshold, released at
// threshold − 5 pp. Tuned to swallow normal Kalman noise on bat_soc
// readings (~0.5–1 pp) without ever flapping the contactor.
const BatSoCUnlockHystPp = 5.0

// Empty reports whether the schedule carries no operator intent. The
// persistence layer writes nothing on Empty so a stale-loadpoint
// schedule on disk is naturally GC'd when the operator clears it via
// the API.
func (s Schedule) Empty() bool {
	return s.SoCPct == 0 && s.TimeOfDayMinUTC == 0 && !s.Recurring && s.SurplusUnlockBatSoCPct == 0
}

// NextDailyUTC returns the next time-of-day deadline (in UTC) strictly
// after `now`. If `now` is already past today's slot, returns
// tomorrow's. Used by RollSchedules to keep recurring deadlines from
// going stale.
//
// `minUTC` is interpreted mod 1440 to defend against UI overflow.
func NextDailyUTC(now time.Time, minUTC int) time.Time {
	minUTC = ((minUTC % 1440) + 1440) % 1440
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(),
		minUTC/60, minUTC%60, 0, 0, time.UTC)
	if !today.After(now) {
		today = today.Add(24 * time.Hour)
	}
	return today
}
