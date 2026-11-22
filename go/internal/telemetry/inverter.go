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

// inverterHeadroomMaxW is the driver guide's bound: a power outside 0 to
// 100 kW is dropped.
const inverterHeadroomMaxW = 100_000

// InverterReading is a driver's latest inverter stream reading.
type InverterReading struct {
	Driver string
	// W is the AC power at the inverter's grid port, in site convention,
	// when the driver sent a finite number.
	W *float64
	// AvailableImportW and AvailableExportW are how much more the inverter
	// will take from, or give to, the grid now, in positive W. Nil when the
	// driver left the field out or sent anything but a number from 0 to
	// 100 kW.
	AvailableImportW *float64
	AvailableExportW *float64
	// Data is the emit as the driver sent it.
	Data      json.RawMessage
	UpdatedAt time.Time
	// Poll is the driver poll the reading came from, as on DerReading.
	Poll uint64
}

// Headroom is how much more the inverter will take from the grid while a
// battery charges, or give to it while one discharges.
func (r InverterReading) Headroom(charge bool) *float64 {
	if charge {
		return r.AvailableImportW
	}
	return r.AvailableExportW
}

// ParseInverterReading reads an inverter emit by the canonical
// @srcful/data-models key names, exactly as Blixt L1 does; power also
// accepts the catalog's "w". Each field stands alone: one that is not a
// usable number is left out and the rest are kept. Only a blob that is not
// a JSON object is an error.
func ParseInverterReading(data json.RawMessage) (InverterReading, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return InverterReading{}, fmt.Errorf("inverter: %w", err)
	}
	w := finiteNumber(fields["W"])
	if w == nil {
		w = finiteNumber(fields["w"])
	}
	return InverterReading{W: w, AvailableImportW: headroomW(fields["available_import_W"]),
		AvailableExportW: headroomW(fields["available_export_W"]), Data: data}, nil
}

// finiteNumber is a JSON number that fits a finite float64, else nil. A
// JSON null stays nil: it never becomes 0.
func finiteNumber(raw json.RawMessage) *float64 {
	var v *float64
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil || !finite(*v) {
		return nil
	}
	return v
}

func headroomW(raw json.RawMessage) *float64 {
	w := finiteNumber(raw)
	if w == nil || *w < 0 || *w > inverterHeadroomMaxW {
		return nil
	}
	return w
}

// UpdateInverter stores a driver's latest inverter reading, from driver
// poll number poll (0 outside a poll).
func (s *Store) UpdateInverter(driver string, r InverterReading, poll uint64) {
	r.Driver, r.UpdatedAt, r.Poll = driver, time.Now(), poll
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inverters == nil {
		s.inverters = map[string]InverterReading{}
	}
	s.inverters[driver] = r
}

// Inverter returns a driver's latest inverter reading whatever the driver's
// health, for a view that shows its age and whether it is stale. Nothing
// that decides, and no view that shows it as live, may use it.
func (s *Store) Inverter(driver string) (InverterReading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.inverters[driver]
	return r, ok
}

// LiveInverter returns a driver's latest inverter reading only while the
// driver is live and its device reports no fault. A cached reading survives
// its driver going stale, offline or faulted; this one does not.
func (s *Store) LiveInverter(driver string) (InverterReading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveInverterLocked(driver)
}

func (s *Store) liveInverterLocked(driver string) (InverterReading, bool) {
	h, ok := s.health[driver]
	if !ok || !h.TelemetryLive() || h.DeviceFault {
		return InverterReading{}, false
	}
	r, ok := s.inverters[driver]
	return r, ok
}

// LiveBatteryInverter is LiveInverter for a reading that came in the same
// driver poll as the driver's latest battery reading. Polls are matched by
// number, not by time: when one of the two reads fails, the other is left
// from an earlier poll however close in time, and the pair is refused.
// Readings from outside a poll never pair.
func (s *Store) LiveBatteryInverter(driver string) (InverterReading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.liveInverterLocked(driver)
	bat := s.readings[key(driver, DerBattery)]
	if !ok || bat == nil || r.Poll == 0 || r.Poll != bat.Poll {
		return InverterReading{}, false
	}
	return r, true
}
