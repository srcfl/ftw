package control

import (
	"encoding/json"
	"log/slog"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A site meter can answer every poll with the same numbers. Each answer is
// a new sample, so every check that keys on UpdatedAt calls it fresh. In rig
// seed 207537 a meter repeated one sample for 39.9 s; dispatch wound the
// battery from +4 kW to -5 kW on it and raised an EV.
//
// A live meter must show a large move of FTW's own devices. When FTW's
// orders move, the devices' own readings follow, and the meter still repeats
// the same total and phase values long after that, the meter is frozen and
// counts as stale. An order a device does not follow (a full battery, a car
// that does not draw, a charger that has not applied it yet) does not count,
// and moves that cancel each other out do not count, since the meter sees
// only their sum.
const (
	// meterFreezeMoveW is how far FTW's devices must move in sum, by order
	// and by their own readings, from where they stood when the meter last
	// changed. Site meters resolve the total to 1-10 W and phase currents
	// to 0.01-1 A (1 A is 230 W). Batteries follow an order to within tens
	// of watts, and inverters ignore steps below about 100-200 W. 500 W is
	// 50 times the coarsest total step and above every inverter dead band.
	// Each EV step is at least 1380 W.
	meterFreezeMoveW = 500.0
	// meterFreezeAfter is how long a live meter may take to show that move.
	// Swedish HAN and DSMR 4 meters send a reading every 10 s, so a driver
	// polling every second sees up to 10 identical samples by design. A
	// telegram lost to a CRC error leaves the last values in place for 20 s,
	// and FTW makes its large moves right after a telegram that shows a load
	// step. 30 s is three telegram periods, so one lost telegram cannot trip
	// it. Seed 207537's freeze is still caught before its EV rise at 38 s.
	meterFreezeAfter = 30 * time.Second
	// meterFreezeSamples is how many identical samples after the move the
	// verdict needs, so a slow poller is judged on several answers.
	meterFreezeSamples = 3
)

// meterSample is the total and per-phase values a site meter reports.
// Absent fields compare as absent.
type meterSample struct {
	vals [10]float64
	has  [10]bool
}

type meterFreeze struct {
	driver string
	at     time.Time // UpdatedAt of the last sample seen
	sample meterSample
	// devices are FTW's devices when the sample last changed.
	devices map[ownDeviceKey]ownDevice
	movedAt time.Time // when the devices first moved past meterFreezeMoveW
	repeats int       // identical samples since movedAt
	// repeatAfterMove: the sample at `at` repeats the values from before
	// the devices moved past meterFreezeMoveW. See siteMeterRepeatsAfterMove.
	repeatAfterMove bool
	frozen          bool
}

// ObserveSiteMeterFreeze compares the site meter's newest sample with the
// one before it, and FTW's own devices with where they stood when the meter
// last changed. Call it every control tick under the control lock, before
// the dispatch gate reads SiteMeterFrozen. A quiet site whose devices do not
// move is never frozen, and any change in the meter's values clears it.
func (s *State) ObserveSiteMeterFreeze(store *telemetry.Store, now time.Time) {
	if s == nil || store == nil {
		return
	}
	f := &s.meterFreeze
	w, data, at, ok := store.OnlineMeterSample(s.SiteMeterDriver)
	if !ok || f.driver != s.SiteMeterDriver {
		// An offline meter is stale on its own; start over once it returns.
		*f = meterFreeze{driver: s.SiteMeterDriver}
		if !ok {
			return
		}
	}
	// No new sample: siteMeterHeld and the watchdog handle that.
	if !at.After(f.at) {
		return
	}
	sample := readMeterSample(w, data)
	devices := s.ownDevices(store)
	if f.at.IsZero() || sample != f.sample {
		if f.frozen {
			slog.Info("site meter values move again", "site_meter", f.driver)
		}
		*f = meterFreeze{driver: f.driver, at: at, sample: sample, devices: devices}
		return
	}
	f.at = at
	if f.frozen {
		return // only a change in the meter's values clears it
	}
	moved := devicesMovedW(f.devices, devices)
	if math.Abs(moved) <= meterFreezeMoveW {
		// Not moved, or moved back: a live meter may repeat itself here.
		f.movedAt, f.repeats, f.repeatAfterMove = time.Time{}, 0, false
		return
	}
	f.repeatAfterMove = true
	if f.movedAt.IsZero() {
		f.movedAt = now
		return
	}
	f.repeats++
	if f.repeats >= meterFreezeSamples && now.Sub(f.movedAt) >= meterFreezeAfter {
		f.frozen = true
		slog.Warn("site meter repeats one reading while FTW's devices moved; treating it as stale",
			"site_meter", f.driver, "devices_moved_w", math.Round(moved),
			"since", now.Sub(f.movedAt).Round(time.Second), "samples", f.repeats)
	}
}

// SiteMeterFrozen reports whether ObserveSiteMeterFreeze found the site
// meter repeating one reading while FTW's devices moved.
func (s *State) SiteMeterFrozen() bool {
	return s != nil && s.meterFreeze.frozen
}

// siteMeterRepeatsAfterMove reports whether meter is the sample
// ObserveSiteMeterFreeze judged last, and it repeats the values from before
// FTW's devices moved by more than meterFreezeMoveW. A live meter shows
// such a move, so the repeat carries no new error. Battery dispatch treats
// it as a held sample (siteMeterHeld) from the first repeat, not only once
// the meter counts as stale. On a meter that sends a reading every 10 s,
// this waits for the next reading instead of acting on the old one again.
func (s *State) siteMeterRepeatsAfterMove(meter *telemetry.DerReading) bool {
	f := &s.meterFreeze
	return meter != nil && f.driver == s.SiteMeterDriver && f.repeatAfterMove && meter.UpdatedAt.Equal(f.at)
}

type ownDeviceKey struct {
	t    telemetry.DerType
	name string
}

// ownDevice is one battery or charger: FTW's last order to it and its own
// latest reading.
type ownDevice struct {
	orderW, readW float64
}

// ownDevices are every battery and charger that reports a reading. One FTW
// has no order for counts as ordered to 0 W: a battery before FTW's first
// order after a start, a charger with no offer (unplugged, or its offer not
// yet known). So a meter frozen from the start, or a car plugged in during
// a freeze, is judged like any other move.
func (s *State) ownDevices(store *telemetry.Store) map[ownDeviceKey]ownDevice {
	out := make(map[ownDeviceKey]ownDevice)
	for driver, w := range store.OnlineRawWByType(telemetry.DerBattery) {
		out[ownDeviceKey{telemetry.DerBattery, driver}] = ownDevice{orderW: s.PrevTargets[driver], readW: w}
	}
	for driver, w := range store.OnlineRawWByType(telemetry.DerEV) {
		out[ownDeviceKey{telemetry.DerEV, driver}] = ownDevice{orderW: s.evCommanded[driver], readW: w}
	}
	return out
}

// devicesMovedW is the net power FTW moved its devices by between a and b,
// in the site's sign, which is what the meter should show. Each device
// counts the smaller of its order's move and its reading's move, in the
// reading's direction, so only an order the device followed counts. A
// device that moved against its order, or was absent from a, counts 0.
func devicesMovedW(a, b map[ownDeviceKey]ownDevice) float64 {
	moved := 0.0
	for k, now := range b {
		then, ok := a[k]
		if !ok {
			continue
		}
		order, read := now.orderW-then.orderW, now.readW-then.readW
		if order*read <= 0 {
			continue
		}
		moved += math.Copysign(min(math.Abs(order), math.Abs(read)), read)
	}
	return moved
}

func readMeterSample(w float64, data json.RawMessage) meterSample {
	var d struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
		L1W *float64 `json:"l1_w"`
		L2W *float64 `json:"l2_w"`
		L3W *float64 `json:"l3_w"`
		L1V *float64 `json:"l1_v"`
		L2V *float64 `json:"l2_v"`
		L3V *float64 `json:"l3_v"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &d)
	}
	var out meterSample
	out.vals[0], out.has[0] = w, true
	for i, p := range [...]*float64{d.L1A, d.L2A, d.L3A, d.L1W, d.L2W, d.L3W, d.L1V, d.L2V, d.L3V} {
		if p != nil {
			out.vals[i+1], out.has[i+1] = *p, true
		}
	}
	return out
}
