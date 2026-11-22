package telemetry

import (
	"encoding/json"
	"fmt"
	"time"
)

// The inverter stream is a battery or hybrid driver's AC stage, sent with
// host.emit("inverter", …) (srcfl/device-drivers spec/host-api-profile.json,
// "streams_vs_ders"): its AC power, frequency, phases, rating, and how much
// more it will take from or give to the grid now. It is not a DER. Its power
// is energy the driver's battery and PV readings already carry, seen at the
// AC port, so it is kept apart from the DER readings: no site sum, dispatch,
// fuse check, energy ledger or history total reads it, nothing of it is
// written to history, and it does not count as a sign of life for the
// driver. Only the latest reading per driver is kept, in memory.

// InverterPollSlack is how far apart an inverter reading and the battery
// reading may arrive and still belong to one poll. A driver emits both
// within milliseconds; a driver whose inverter read failed leaves the
// reading of an earlier poll, a poll interval older.
const InverterPollSlack = 2 * time.Second

// inverterHeadroomMaxW is the driver guide's bound: a power outside 0 to
// 100 kW is dropped.
const inverterHeadroomMaxW = 100_000

// InverterReading is a driver's latest inverter stream reading.
type InverterReading struct {
	Driver string
	// W is the AC power at the inverter's grid port, in site convention,
	// when the driver sent it.
	W *float64
	// AvailableImportW and AvailableExportW are how much more the inverter
	// will take from, or give to, the grid now, in positive W. Nil when the
	// driver left the field out or sent a value outside 0 to 100 kW.
	AvailableImportW *float64
	AvailableExportW *float64
	// Data is the emit as the driver sent it.
	Data      json.RawMessage
	UpdatedAt time.Time
}

// Headroom is how much more the inverter will take from the grid while a
// battery charges, or give to it while one discharges.
func (r InverterReading) Headroom(charge bool) *float64 {
	if charge {
		return r.AvailableImportW
	}
	return r.AvailableExportW
}

// ParseInverterReading reads an inverter emit. The headroom keys are the
// canonical @srcful/data-models names; power accepts the catalog's "w" too.
func ParseInverterReading(data json.RawMessage) (InverterReading, error) {
	var d struct {
		W                *float64 `json:"W"`
		LegacyW          *float64 `json:"w"`
		AvailableImportW any      `json:"available_import_W"`
		AvailableExportW any      `json:"available_export_W"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return InverterReading{}, fmt.Errorf("inverter: %w", err)
	}
	w := d.W
	if w == nil {
		w = d.LegacyW
	}
	if w != nil && !finite(*w) {
		return InverterReading{}, fmt.Errorf("inverter power is non-finite: %v", *w)
	}
	return InverterReading{W: w, AvailableImportW: headroomW(d.AvailableImportW), AvailableExportW: headroomW(d.AvailableExportW),
		Data: data}, nil
}

func headroomW(v any) *float64 {
	w, ok := v.(float64)
	if !ok || !finite(w) || w < 0 || w > inverterHeadroomMaxW {
		return nil
	}
	return &w
}

// UpdateInverter stores a driver's latest inverter reading.
func (s *Store) UpdateInverter(driver string, r InverterReading) {
	r.Driver, r.UpdatedAt = driver, time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inverters == nil {
		s.inverters = map[string]InverterReading{}
	}
	s.inverters[driver] = r
}

// Inverter returns a driver's latest inverter reading whatever the driver's
// health, for views that show the last known value with its age. Nothing
// that decides may use it; see LiveBatteryInverter.
func (s *Store) Inverter(driver string) (InverterReading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.inverters[driver]
	return r, ok
}

// LiveBatteryInverter returns the inverter reading that came in the same
// poll as the driver's latest battery reading, while the driver is live
// and its device reports no fault. A cached reading survives its driver
// going stale, offline or faulted, and an inverter read that failed leaves
// an older reading behind; neither is returned.
func (s *Store) LiveBatteryInverter(driver string) (InverterReading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.health[driver]
	if !ok || !h.TelemetryLive() || h.DeviceFault {
		return InverterReading{}, false
	}
	r, ok := s.inverters[driver]
	bat := s.readings[key(driver, DerBattery)]
	if !ok || bat == nil {
		return InverterReading{}, false
	}
	if apart := r.UpdatedAt.Sub(bat.UpdatedAt); apart > InverterPollSlack || apart < -InverterPollSlack {
		return InverterReading{}, false
	}
	return r, true
}
