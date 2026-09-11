package loadpoint

import (
	"encoding/json"
	"time"

	"github.com/srcfl/ftw/go/internal/units"
)

// Schedule is the user's persistent charging intent for one loadpoint:
// "be at SoC by TimeOfDayMinUTC each day". When Recurring is true the
// Manager rolls the loadpoint's targetTime forward to tomorrow once
// today's deadline passes; when false the schedule still hydrates the
// one-shot target_soc_pct/target_time fields on save but doesn't refresh
// itself.
//
// SurplusUnlockBatSoC, if > 0, tells the dispatch controller to grab
// PV surplus into this loadpoint whenever the home battery's SoC sits at
// or above the threshold — even when SurplusOnly is off and the MPC has
// nothing planned. Hysteresis (release at threshold − BatSoCUnlockHyst)
// keeps the contactor from flapping at the boundary.
//
// Zero value (Empty) means "no schedule configured". Persistence keys
// off this — Empty schedules are not written to disk.
type Schedule struct {
	SoC             float64 `json:"soc"`
	TimeOfDayMinUTC int     `json:"time_of_day_min_utc"` // 0..1439
	Recurring       bool    `json:"recurring"`
	// Days restricts which weekdays the deadline may land on: a 7-bit
	// mask, bit 0 = Monday through bit 6 = Sunday (ISO order). Zero
	// means every day — the value every schedule stored before this
	// field existed decodes to, so old rows and old clients keep
	// their behaviour. The weekday is the household's, not UTC's: the
	// mask is read in the box's own time zone (see NextDeadlineUTC).
	Days                uint8   `json:"days,omitempty"`
	SurplusUnlockBatSoC float64 `json:"surplus_unlock_bat_soc,omitempty"`
}

// BatSoCUnlockHyst is the fraction gap between arm and release for the
// bat-SoC surplus unlock. Armed at threshold, released at threshold −
// 0.05. Tuned to swallow normal Kalman noise on bat SoC (~0.005–0.01)
// without ever flapping the contactor.
const BatSoCUnlockHyst = 0.05

// Normalize folds a persisted or inbound schedule onto 0–1 fractions.
// Old rows stored soc_pct as 0–100; new rows store soc as 0–1.
func (s *Schedule) Normalize() {
	if s == nil {
		return
	}
	s.SoC = units.ClampFraction(units.FractionFromLegacyPercent(s.SoC))
	s.SurplusUnlockBatSoC = units.ClampFraction(units.FractionFromLegacyPercent(s.SurplusUnlockBatSoC))
}

func (s *Schedule) UnmarshalJSON(b []byte) error {
	type alias Schedule
	aux := struct {
		alias
		SoCPct                 *float64 `json:"soc_pct"`
		SurplusUnlockBatSoCPct *float64 `json:"surplus_unlock_bat_soc_pct"`
	}{}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*s = Schedule(aux.alias)
	if s.SoC == 0 && aux.SoCPct != nil {
		s.SoC = *aux.SoCPct
	}
	if s.SurplusUnlockBatSoC == 0 && aux.SurplusUnlockBatSoCPct != nil {
		s.SurplusUnlockBatSoC = *aux.SurplusUnlockBatSoCPct
	}
	s.Normalize()
	return nil
}

// HasTarget reports whether the schedule commits to a SoC by a deadline.
// A target makes the plan the floor of automatic dispatch: the runtime
// surplus clamps may add to it but never throttle it (see
// Controller.surplusActive and surplusAddsToPlan, and the planner spec
// gate in main.go).
func (s Schedule) HasTarget() bool { return s.SoC > 0 }

// Empty reports whether the schedule carries no operator intent. The
// persistence layer writes nothing on Empty so a stale-loadpoint
// schedule on disk is naturally GC'd when the operator clears it via
// the API.
func (s Schedule) Empty() bool {
	return s.SoC == 0 && s.TimeOfDayMinUTC == 0 && !s.Recurring && s.SurplusUnlockBatSoC == 0
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

// NextDeadlineUTC returns the schedule's next deadline strictly after
// `now`: the stored time-of-day (UTC minutes, via NextDailyUTC) on the
// next day whose bit is set in Days. The weekday is read in `loc` — the
// box's own zone — because "weekdays" must mean the household's
// weekdays, not UTC's: a 00:30 Saturday deadline in Stockholm is still
// Friday in UTC, and a mask read in UTC would skip the wrong day. A
// zero mask means every day; nil loc falls back to time.Local, which
// is the box's zone.
//
// Known drift: the time-of-day itself stays stored as UTC minutes, so
// when the household crosses a DST change the local wall-clock deadline
// shifts by an hour until the operator re-saves. Storing local minutes
// instead needs a migration and a UI save-path change; that is deferred
// on purpose rather than half-done here.
func (s Schedule) NextDeadlineUTC(now time.Time, loc *time.Location) time.Time {
	next := NextDailyUTC(now, s.TimeOfDayMinUTC)
	days := s.Days & 0x7F
	if days == 0 {
		return next
	}
	if loc == nil {
		loc = time.Local
	}
	for range 7 {
		// time.Weekday counts Sunday=0; the mask counts Monday=0 (ISO).
		iso := (int(next.In(loc).Weekday()) + 6) % 7
		if days&(1<<iso) != 0 {
			break
		}
		// Adding 24 h in UTC keeps the stored time-of-day fixed; only
		// the weekday moves. A non-zero 7-bit mask matches within the
		// 7 candidates this loop examines.
		next = next.Add(24 * time.Hour)
	}
	return next
}
