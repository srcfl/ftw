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
// that does not draw, a charger that has not applied it yet) does not count.
const (
	// meterFreezeMoveW is how far FTW's devices must move, by order and by
	// their own readings, from where they stood when the meter last
	// changed. Site meters resolve the total to 1-10 W and phase currents
	// to 0.01-1 A (1 A is 230 W). Batteries follow an order to within tens
	// of watts, and inverters ignore steps below about 100-200 W. 500 W is
	// 50 times the coarsest total step and above every inverter dead band.
	// Each EV step is at least 1380 W.
	meterFreezeMoveW = 500.0
	// meterFreezeAfter is how long a live meter may take to show that move.
	// Swedish HAN and DSMR 4 meters send a reading every 10 s, so a driver
	// polling every second sees up to 10 identical samples by design. The
	// device readings already show the move, so only the meter's own
	// refresh and a poll remain; 15 s covers both.
	meterFreezeAfter = 15 * time.Second
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
	frozen  bool
}

// ObserveSiteMeterFreeze compares the site meter's newest sample with the
// one before it, and FTW's own devices with where they stood when the meter
// last changed. Call it every control tick under the control lock, before
// the dispatch gate reads SiteMeterFrozen. A quiet site whose devices do not
// move is never frozen, and any change in the meter's values clears it.
func (s *State) ObserveSiteMeterFreeze(store *telemetry.Store) {
	if s == nil || store == nil {
		return
	}
	f := &s.meterFreeze
	r := store.Get(s.SiteMeterDriver, telemetry.DerMeter)
	if r == nil || f.driver != s.SiteMeterDriver {
		*f = meterFreeze{driver: s.SiteMeterDriver}
		if r == nil {
			return
		}
	}
	// No new sample: siteMeterHeld and the watchdog handle that.
	if !r.UpdatedAt.After(f.at) {
		return
	}
	sample := readMeterSample(r)
	devices := s.ownDevices(store)
	if f.at.IsZero() || sample != f.sample {
		if f.frozen {
			slog.Info("site meter values move again", "site_meter", f.driver)
		}
		*f = meterFreeze{driver: f.driver, at: r.UpdatedAt, sample: sample, devices: devices}
		return
	}
	f.at = r.UpdatedAt
	now := s.now()
	if f.movedAt.IsZero() {
		if devicesMovedW(f.devices, devices) > meterFreezeMoveW {
			f.movedAt = now
		}
		return
	}
	f.repeats++
	if !f.frozen && f.repeats >= meterFreezeSamples && now.Sub(f.movedAt) >= meterFreezeAfter {
		f.frozen = true
		slog.Warn("site meter repeats one reading while FTW's devices moved; treating it as stale",
			"site_meter", f.driver, "devices_moved_w", math.Round(devicesMovedW(f.devices, devices)),
			"since", now.Sub(f.movedAt).Round(time.Second), "samples", f.repeats)
	}
}

// SiteMeterFrozen reports whether ObserveSiteMeterFreeze found the site
// meter repeating one reading while FTW's devices moved.
func (s *State) SiteMeterFrozen() bool {
	return s != nil && s.meterFreeze.frozen
}

type ownDeviceKey struct {
	t    telemetry.DerType
	name string
}

// ownDevice is one battery or charger FTW orders: its last order and its
// own latest reading.
type ownDevice struct {
	orderW, readW float64
	read          bool
}

// ownDevices are the batteries and chargers FTW has sent orders to.
func (s *State) ownDevices(store *telemetry.Store) map[ownDeviceKey]ownDevice {
	out := make(map[ownDeviceKey]ownDevice, len(s.PrevTargets)+len(s.evCommanded))
	add := func(name string, t telemetry.DerType, orderW float64) {
		d := ownDevice{orderW: orderW}
		if r := store.Get(name, t); r != nil {
			d.readW, d.read = r.RawW, true
		}
		out[ownDeviceKey{t, name}] = d
	}
	for name, w := range s.PrevTargets {
		add(name, telemetry.DerBattery, w)
	}
	for name, w := range s.evCommanded {
		add(name, telemetry.DerEV, w)
	}
	return out
}

// devicesMovedW is how far FTW moved its devices between a and b. Each
// device counts the smaller of its order's move and its reading's move, so
// only an order the device followed counts.
func devicesMovedW(a, b map[ownDeviceKey]ownDevice) float64 {
	moved := 0.0
	for k, now := range b {
		then, ok := a[k]
		if !ok || !then.read || !now.read {
			continue
		}
		moved += min(math.Abs(now.orderW-then.orderW), math.Abs(now.readW-then.readW))
	}
	return moved
}

func readMeterSample(r *telemetry.DerReading) meterSample {
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
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &d)
	}
	var out meterSample
	out.vals[0], out.has[0] = r.RawW, true
	for i, p := range [...]*float64{d.L1A, d.L2A, d.L3A, d.L1W, d.L2W, d.L3W, d.L1V, d.L2V, d.L3V} {
		if p != nil {
			out.vals[i+1], out.has[i+1] = *p, true
		}
	}
	return out
}
