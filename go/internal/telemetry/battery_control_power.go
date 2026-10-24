package telemetry

import "time"

// OnlineBatteryControlPower qualifies battery delivery before control may
// release the reservation for an import increase already commanded.
func (s *Store) OnlineBatteryControlPower(driver string, now time.Time, maxAge time.Duration) (ControlObservation, bool) {
	return s.onlineControlPower(driver, DerBattery, now, maxAge)
}
