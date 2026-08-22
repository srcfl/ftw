package mpc

import (
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// LoadpointDirective is the charger slice of this slot. main.go and the
// site-clock tests both send this to loadpoint.Controller so EV energy
// budgets cannot drift from the plan the battery dispatch sees.
func (d SlotDirective) LoadpointDirective() loadpoint.Directive {
	return loadpoint.Directive{
		SlotStart:         d.SlotStart,
		SlotEnd:           d.SlotEnd,
		LoadpointEnergyWh: d.LoadpointEnergyWh,
	}
}

// PeakPlannedSurplusForEV is the near-term 3Φ-gate scan: peak leftover
// PV after house load, minus planned PV-soak battery charge, over
// slots that overlap [now, now+window]. Soak uses GridW minus EV so
// soak+EV import is not treated as the battery buying.
func PeakPlannedSurplusForEV(actions []Action, now time.Time, window time.Duration) (float64, bool) {
	if len(actions) == 0 {
		return 0, false
	}
	horizon := now.Add(window)
	var peak float64
	any := false
	for _, a := range actions {
		slotEnd := time.UnixMilli(a.SlotStartMs).Add(time.Duration(a.SlotLenMin) * time.Minute)
		if slotEnd.Before(now) {
			continue
		}
		if time.UnixMilli(a.SlotStartMs).After(horizon) {
			break
		}
		surplus := loadpoint.PlannedSurplusForEVW(a.LoadW, a.PVW, a.BatteryW, a.GridW-a.LoadpointW)
		if !any || surplus > peak {
			peak = surplus
			any = true
		}
	}
	return peak, any
}
