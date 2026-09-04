package prices

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// StaticProvider synthesises a day of prices from a configured flat rate
// or time-of-use schedule. It is the worldwide stand-in for markets that
// have no day-ahead feed: a US TOU tariff, a Japanese flat contract, or
// any other schedule the operator can type.
//
// Rates are minor units per kWh of the install currency (the same unit as
// grid_tariff_ore_kwh). Fetch converts them to major units so Apply can
// turn them back — the same path every other provider uses.
type StaticProvider struct {
	OreKwh float64
	TOU    []config.TOUWindow
	// Loc is the timezone windows are interpreted in. Nil means the box's
	// local zone, which is what a household tariff is billed in.
	Loc *time.Location
}

func NewStatic(cfg *config.Price) *StaticProvider {
	if cfg == nil {
		return &StaticProvider{}
	}
	tou := append([]config.TOUWindow(nil), cfg.StaticTOU...)
	return &StaticProvider{OreKwh: cfg.StaticOreKwh, TOU: tou}
}

func (s *StaticProvider) Name() string { return "static" }

func (s *StaticProvider) loc() *time.Location {
	if s != nil && s.Loc != nil {
		return s.Loc
	}
	return time.Local
}

// Fetch returns 96 quarter-hour slots for the civil day of `day` in the
// provider's location. Zone is ignored: a static tariff is not a bidding
// zone. An empty result is never returned — a configured static source
// always has a price, including zero.
func (s *StaticProvider) Fetch(_ context.Context, _ string, day time.Time) ([]RawPrice, error) {
	loc := s.loc()
	start := time.Date(day.In(loc).Year(), day.In(loc).Month(), day.In(loc).Day(), 0, 0, 0, 0, loc)
	const slotMin = 15
	out := make([]RawPrice, 0, 96)
	for i := 0; i < 96; i++ {
		t := start.Add(time.Duration(i*slotMin) * time.Minute)
		ore, err := s.rateAt(t)
		if err != nil {
			return nil, err
		}
		out = append(out, RawPrice{
			SlotStart:  t,
			SlotLenMin: slotMin,
			SEKPerKWh:  ore / 100,
		})
	}
	return out, nil
}

func (s *StaticProvider) rateAt(t time.Time) (float64, error) {
	t = t.In(s.loc())
	mins := t.Hour()*60 + t.Minute()
	dow := strings.ToLower(t.Weekday().String()[:3]) // sun, mon, …
	for _, w := range s.TOU {
		ok, err := windowMatches(w, mins, dow)
		if err != nil {
			return 0, err
		}
		if ok {
			return w.OreKwh, nil
		}
	}
	return s.OreKwh, nil
}

func windowMatches(w config.TOUWindow, mins int, dow string) (bool, error) {
	if !daysMatch(w.Days, dow) {
		return false, nil
	}
	start, err := parseClockMinutes(w.Start)
	if err != nil {
		return false, fmt.Errorf("static TOU start %q: %w", w.Start, err)
	}
	end, err := parseClockMinutes(w.End)
	if err != nil {
		return false, fmt.Errorf("static TOU end %q: %w", w.End, err)
	}
	if start == end {
		return false, fmt.Errorf("static TOU window %s–%s is empty", w.Start, w.End)
	}
	if start < end {
		return mins >= start && mins < end, nil
	}
	// Overnight: 22:00–06:00.
	return mins >= start || mins < end, nil
}

func daysMatch(days []string, dow string) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		if dayToken(d) == dow {
			return true
		}
	}
	return false
}

func dayToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "sunday", "sun":
		return "sun"
	case "monday", "mon":
		return "mon"
	case "tuesday", "tue", "tues":
		return "tue"
	case "wednesday", "wed":
		return "wed"
	case "thursday", "thu", "thur", "thurs":
		return "thu"
	case "friday", "fri":
		return "fri"
	case "saturday", "sat":
		return "sat"
	default:
		if len(s) >= 3 {
			return s[:3]
		}
		return s
	}
}

// parseClockMinutes reads "HH:MM" or "H:MM". "24:00" is midnight at the
// end of the day (1440), so a window can close on the civil day boundary.
func parseClockMinutes(s string) (int, error) {
	s = strings.TrimSpace(s)
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err != nil || n != 2 {
		return 0, fmt.Errorf("want HH:MM")
	}
	if h == 24 && m == 0 {
		return 24 * 60, nil
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("hour 0–23 and minute 0–59, or 24:00")
	}
	return h*60 + m, nil
}
