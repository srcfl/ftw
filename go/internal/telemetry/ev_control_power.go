package telemetry

import "time"

// OnlineEVControlPower qualifies raw EV delivery for optional battery-load
// reservation. A faulted charger earns no credit here (unlike total site-load
// accounting), since retaining the full reservation is the conservative choice.
func (s *Store) OnlineEVControlPower(driver string, now time.Time, maxAge time.Duration) (ControlObservation, bool) {
	return s.onlineControlPower(driver, DerEV, now, maxAge)
}
