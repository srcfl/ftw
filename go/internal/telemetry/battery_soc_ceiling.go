package telemetry

import "encoding/json"

// BatterySoCCeiling is the highest state of charge, 0..1, that a battery's
// own device reports it will charge to. Drivers send it as max_soc_fract on
// the battery reading, the name @srcful/data-models gives a battery's own
// maximum SoC. A Pixii can stop at 90 % while Core plans to 95 %
// (srcfl/device-drivers#175). A missing, non-finite, zero or out-of-range
// value reports false.
func BatterySoCCeiling(data json.RawMessage) (float64, bool) {
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
