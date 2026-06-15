package flexload

// ExternalHeatDetector spots a heat source the EMS doesn't control and isn't
// paying for — most importantly a wood stove / fireplace (kamin), but also
// strong solar gain or a gas heater. The signature is a fast indoor
// temperature rise that the thermal model can't explain from the electrical
// heating it knows about: observed warming ≫ model-predicted warming while
// the metered heating power is ~off.
//
// Why it matters: when the living room is being heated for free, running
// 42W-controlled electric heat in the same zone is pure waste. On detection
// the scheduler pauses those electric sources (frost-protection setpoint).
// Just as important, the model must NOT train through a firing — the extra
// heat would be misattributed to the building's own dynamics and corrupt the
// learned time constant — so detection also gates training off.
//
// The detector learns each firing's heat amount and the typical per-cycle
// energy, so over time 42W knows roughly how much "free" heat a firing
// delivers and how long to keep electric heat paused.
type ExternalHeatDetector struct {
	// Learned, persisted.
	EstThermalW float64 `json:"est_thermal_w"` // EMA of inferred external power while active (W)
	AvgCycleWh  float64 `json:"avg_cycle_wh"`  // EMA of energy delivered per firing (Wh)
	Cycles      int64   `json:"cycles"`        // completed firings observed

	// Transient (not meaningfully persisted; re-derived within minutes).
	active        bool
	sinceMs       int64
	lastDetectMs  int64
	cycleEnergyWh float64
}

const (
	// Unexplained warming rate (°C/h) above which we infer an external source.
	extHeatTriggerCPerH = 0.4
	// Our own metered heating must be at/below this (W) — otherwise the
	// warming is plausibly just our electric heat doing its job.
	extHeatMaxMeteredW = 100.0
	// Keep "active" this long after the last positive detection so brief
	// dips between detections don't flap the pause on and off (ms).
	extHeatHoldMs = 20 * 60 * 1000
	// Ignore firings that delivered less than this (Wh) as noise.
	extHeatMinCycleWh = 50.0
)

// Update folds one observed transition into the detector.
//
//	observedDeltaC — actual indoor temp change over the step
//	expectedDeltaC — model-predicted change for the same step + known heat
//	meteredHeatW   — our own electrical heating over the step (W)
//	dtS            — step length (s)
//	thermalWForRate — converts an unexplained °C/s rate into external W
//	                  (typically model.ThermalWForRate)
func (e *ExternalHeatDetector) Update(
	observedDeltaC, expectedDeltaC, meteredHeatW, dtS float64,
	nowMs int64,
	thermalWForRate func(rateCPerS float64) float64,
) {
	if dtS <= 0 {
		return
	}
	unexplainedRate := (observedDeltaC - expectedDeltaC) / dtS // °C/s
	detected := unexplainedRate*3600 > extHeatTriggerCPerH && meteredHeatW <= extHeatMaxMeteredW

	if detected {
		if w := thermalWForRate(unexplainedRate); w > 0 {
			if e.EstThermalW == 0 {
				e.EstThermalW = w
			} else {
				e.EstThermalW = 0.9*e.EstThermalW + 0.1*w
			}
			e.cycleEnergyWh += w * dtS / 3600.0
		}
		if !e.active {
			e.active = true
			e.sinceMs = nowMs
		}
		e.lastDetectMs = nowMs
		return
	}

	// No detection this step — close the cycle once the hold window lapses.
	if e.active && nowMs-e.lastDetectMs > extHeatHoldMs {
		e.active = false
		if e.cycleEnergyWh >= extHeatMinCycleWh {
			if e.AvgCycleWh == 0 {
				e.AvgCycleWh = e.cycleEnergyWh
			} else {
				e.AvgCycleWh = 0.7*e.AvgCycleWh + 0.3*e.cycleEnergyWh
			}
			e.Cycles++
		}
		e.cycleEnergyWh = 0
	}
}

// FiringSinceMs returns the timestamp the current firing was first detected
// (0 when not active), so callers can estimate elapsed firing time.
func (e *ExternalHeatDetector) FiringSinceMs() int64 { return e.sinceMs }

// Active reports whether an external heat source is currently firing (within
// the hold window of the last detection).
func (e *ExternalHeatDetector) Active(nowMs int64) bool {
	if !e.active {
		return false
	}
	// Self-heal if the service missed the closing sample (e.g. driver outage):
	// treat a long-stale detection as inactive.
	return nowMs-e.lastDetectMs <= extHeatHoldMs
}
