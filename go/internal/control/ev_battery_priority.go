package control

import (
	"log/slog"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evBatteryYield lets an existing car offer wait for a battery charge cut.
// It never grants a car rise, never runs above the fuse's working limit
// (fuseLineA), and cannot renew while the site remains over its working limit.
type evBatteryYield struct {
	tried      bool
	until      time.Time
	active     bool
	maxChargeW float64
}

func (s *State) hasBatteryCharge() bool {
	for _, log := range s.batteryCommands {
		if _, _, latest, ok := log.span(s.now()); ok && latest > 0 {
			return true
		}
	}
	return false
}

func (s *State) yieldBatteryBeforeEVCut(store *telemetry.Store, meter *telemetry.DerReading, r evMeterRoom, pool evPool, requiredW, gridW, pendingBatteryW float64, rise pendingLoad, fresh bool) bool {
	y := &s.evLimit.batteryYield
	if !fresh || !r.perPhase || pool.offerW <= 0 || requiredW <= 0 || s.SiteFuseVoltage <= 0 || s.SiteFusePhases <= 0 {
		return false
	}
	// This hold uses only the installation's existing current allowance.
	// Include offers still on their way; a cut never funds a car increase.
	amps, present, valid := phaseCurrentsA(meter, s, gridW)
	lineA := s.fuseLineA()
	if !present || !valid || gridW+rise.w+pendingBatteryW > lineA*s.SiteFuseVoltage*float64(s.SiteFusePhases) {
		return false
	}
	pendingA := pendingBatteryW / (s.SiteFuseVoltage * float64(s.SiteFusePhases))
	for i, a := range amps {
		if math.Max(a, math.Sqrt(s.fuseHeat.sq[i]))+rise.phaseA[i]+pendingA > lineA {
			return false
		}
	}
	var available, commanded float64
	for driver, log := range s.batteryCommands {
		h := store.DriverHealth(driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		_, high, latest, ok := log.span(s.now())
		if ok && finiteFuseValue(high) && finiteFuseValue(latest) {
			available += math.Max(0, high)
			commanded += math.Max(0, latest)
		}
	}
	if available+1e-6 < requiredW {
		return false
	}
	// Removing balanced charge must not overload an exporting phase.
	cutA := requiredW / (s.SiteFuseVoltage * float64(s.SiteFusePhases))
	for _, a := range amps {
		if a-cutA < -s.SiteFuseAmps {
			return false
		}
	}
	if !y.tried {
		y.tried, y.until = true, s.now().Add(evSettle)
		slog.Info("EV priority: battery charging yields before the car",
			"battery_reduction_w", requiredW, "car_offer_w", pool.offerW, "until", y.until)
	}
	if !s.now().Before(y.until) {
		return false
	}
	y.active, y.maxChargeW = true, math.Max(0, commanded-requiredW)
	return true
}

func (s *State) applyEVBatteryYield(targets []DispatchTarget) []DispatchTarget {
	if !s.evLimit.batteryYield.active || !s.now().Before(s.evLimit.batteryYield.until) {
		return targets
	}
	var chargeW float64
	for _, target := range targets {
		chargeW += math.Max(0, target.TargetW)
	}
	capW := s.evLimit.batteryYield.maxChargeW
	if chargeW <= capW || chargeW <= 0 {
		return targets
	}
	for i := range targets {
		if targets[i].TargetW > 0 {
			targets[i].TargetW *= capW / chargeW
			targets[i].Clamped = true
		}
	}
	return targets
}
