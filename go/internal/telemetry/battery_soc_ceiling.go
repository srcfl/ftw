package telemetry

import (
	"encoding/json"
	"log/slog"
	"time"
)

// A battery's own charge ceiling is the highest state of charge its device
// reports it will charge to. Drivers send it as max_soc_fract on the battery
// reading, the name @srcful/data-models gives a battery's own maximum SoC. A
// Pixii can stop at 90 % while Core plans to 95 % (srcfl/device-drivers#175).

// MinBatterySoCCeiling is the lowest device ceiling Core trusts. Real
// ceilings sit around 80-100 %; a misread register that says 30 % would
// otherwise stop all charging above 30 %.
const MinBatterySoCCeiling = 0.5

// BatterySoCCeilingHold is how long a ceiling outlives readings that no
// longer carry it. A limit register that fails a read leaves the ceiling out
// of that reading, and the Pixii driver retries a register that missed three
// polls only after five minutes. Dropping the ceiling at once would release
// a full battery for that time.
const BatterySoCCeilingHold = 10 * time.Minute

// SoCCeilingNow is the battery's own charge ceiling, 0..1, while the reading
// is current (BatterySoCMaxAge) and the ceiling was reported within
// BatterySoCCeilingHold.
func (r *DerReading) SoCCeilingNow(now time.Time) (float64, bool) {
	if r == nil || r.SoCCeiling == nil ||
		now.Sub(r.UpdatedAt) > BatterySoCMaxAge || now.Sub(r.SoCCeilingAt) > BatterySoCCeilingHold {
		return 0, false
	}
	return *r.SoCCeiling, true
}

// parseSoCCeiling reads max_soc_fract. A missing, non-finite, zero or
// out-of-range value reports false.
func parseSoCCeiling(data json.RawMessage) (float64, bool) {
	if len(data) == 0 {
		return 0, false
	}
	var d struct {
		MaxSoCFract *float64 `json:"max_soc_fract"`
	}
	if json.Unmarshal(data, &d) != nil || d.MaxSoCFract == nil {
		return 0, false
	}
	v := *d.MaxSoCFract
	if !finite(v) || v <= 0 || v > 1 {
		return 0, false
	}
	return v, true
}

// socCeilingFor is the ceiling a new battery reading carries: the one in its
// data, else the previous reading's while within BatterySoCCeilingHold. A
// value below MinBatterySoCCeiling is logged once and ignored. Caller holds
// s.mu.
func (s *Store) socCeilingFor(driver string, data json.RawMessage, prev *DerReading, now time.Time) (*float64, time.Time) {
	if v, ok := parseSoCCeiling(data); ok {
		if v >= MinBatterySoCCeiling {
			delete(s.lowSoCCeilings, driver)
			return &v, now
		}
		if logged, seen := s.lowSoCCeilings[driver]; !seen || logged != v {
			if s.lowSoCCeilings == nil {
				s.lowSoCCeilings = map[string]float64{}
			}
			s.lowSoCCeilings[driver] = v
			slog.Warn("telemetry: ignoring a battery charge ceiling below the trusted minimum",
				"driver", driver, "max_soc_fract", v, "min", MinBatterySoCCeiling)
		}
	}
	if prev != nil && prev.SoCCeiling != nil && now.Sub(prev.SoCCeilingAt) <= BatterySoCCeilingHold {
		return prev.SoCCeiling, prev.SoCCeilingAt
	}
	return nil, time.Time{}
}
