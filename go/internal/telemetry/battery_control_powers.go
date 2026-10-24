package telemetry

import "time"

// OnlineBatteryControlPowers returns qualified battery observations for
// diagnostic context. Callers may align them with another source, but cached
// readings alone never establish a live battery contribution.
func (s *Store) OnlineBatteryControlPowers(now time.Time, maxAge time.Duration) []ControlObservation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if maxAge <= 0 {
		return nil
	}
	var out []ControlObservation
	for _, r := range s.readings {
		if r == nil || r.DerType != DerBattery {
			continue
		}
		h := s.health[r.Driver]
		if h == nil || !h.TelemetryLive() || h.DeviceFault || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > maxAge {
			continue
		}
		obs, known := ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
		age := now.Sub(obs.Seen())
		if !known || age < 0 || age > maxAge || age > ControlPowerMaxAge(r.Data) {
			continue
		}
		out = append(out, obs)
	}
	return out
}
