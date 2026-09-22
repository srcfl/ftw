package loadpoint

import "time"

// ChargingPeriod returns fresh observed charging, with the duration of the
// current uninterrupted run. Commanded power is not evidence of charging.
func (m *Manager) ChargingPeriod(id string) (bool, time.Duration) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp, ok := m.byID[id]
	if !ok {
		return false, 0
	}
	now := m.now()
	window := lp.powerWindow
	if window <= 0 {
		window = (EVSample{}).PowerWindow()
	}
	if !lp.pluggedIn || lp.powerUnavailable || lp.currentPowerW < steadyChargeFloorW ||
		lp.powerAt.IsZero() || lp.powerAt.After(now.Add(time.Second)) || now.Sub(lp.powerAt) > window ||
		lp.chargingPeriodSince.IsZero() || lp.chargingPeriodSince.After(now) {
		return false, 0
	}
	return true, now.Sub(lp.chargingPeriodSince)
}

// An observation gap cannot prove uninterrupted charging. Keep this planning
// history separate from the manager's interruption notification hysteresis.
func observeChargingPeriod(lp *loadpointRuntime, sample EVSample, now time.Time) {
	at := sample.PowerAt
	if at.IsZero() {
		at = now
	}
	window := lp.powerWindow
	if window <= 0 {
		window = (EVSample{}).PowerWindow()
	}
	if !sample.Connected || sample.PowerUnavailable || !finite(sample.PowerW) || sample.PowerW < steadyChargeFloorW ||
		at.After(now.Add(time.Second)) || now.Sub(at) > sample.PowerWindow() {
		lp.chargingPeriodSince = time.Time{}
		return
	}
	if lp.chargingPeriodSince.IsZero() || !lp.pluggedIn || at.Before(lp.powerAt) || at.Sub(lp.powerAt) > window {
		lp.chargingPeriodSince = now
	}
}
