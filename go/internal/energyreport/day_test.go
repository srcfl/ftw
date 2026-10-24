package energyreport

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

var day = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

const tick = 2 * time.Minute

type asset struct {
	kind    state.EnergyAssetKind
	flow    state.EnergyFlow
	watts   float64
	counter bool
}

// The test site: a metered grid, solar, a battery and a charger, read every
// two minutes, the grid meter by its counters and the rest by power.
var site = map[string][]asset{
	"meter:1/grid_meter":        {{state.AssetGridMeter, state.FlowGridImport, 1000, true}, {state.AssetGridMeter, state.FlowGridExport, 0, true}},
	"inverter:1/pv":             {{state.AssetPV, state.FlowPVGeneration, 500, false}},
	"battery:1/battery":         {{state.AssetBattery, state.FlowBatteryCharge, 250, false}, {state.AssetBattery, state.FlowBatteryDischarge, 0, false}},
	"charger:1/vehicle_charger": {{state.AssetVehicleCharger, state.FlowVehicleCharge, 1000, false}},
	"site/observed-consumer":    {{state.AssetObservedConsumer, state.FlowConsumerUse, 700, false}},
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// record writes the site from `from` to `to`, in time order and in one
// transaction, leaving out any asset whose id skip returns true for at that
// time. A counter keeps counting while it is not read, as a meter's does.
func record(t *testing.T, st *state.Store, from, to time.Time, skip func(id string, at time.Time) bool) {
	t.Helper()
	var obs []state.EnergyObservation
	for at := from; !at.After(to); at = at.Add(tick) {
		for id, flows := range site {
			if skip != nil && skip(id, at) {
				continue
			}
			for _, a := range flows {
				w, c := a.watts, a.watts*at.Sub(from).Hours()
				o := state.EnergyObservation{AssetID: id, DeviceID: id, AssetKind: a.kind, Label: "x", Flow: a.flow, AtMs: at.UnixMilli(), PowerW: &w}
				if a.counter {
					o.CounterWh, o.PowerW = &c, nil
				}
				obs = append(obs, o)
			}
		}
	}
	if err := st.RecordTickWithOptionalHistory(nil, nil, obs); err != nil {
		t.Fatal(err)
	}
}

// sharedDay is the whole test day, written once and only read.
var sharedDay struct {
	once sync.Once
	dir  string
	st   *state.Store
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDay.st != nil {
		_ = sharedDay.st.Close()
	}
	if sharedDay.dir != "" {
		_ = os.RemoveAll(sharedDay.dir)
	}
	os.Exit(code)
}

func whole(t *testing.T) *state.Store {
	t.Helper()
	sharedDay.once.Do(func() {
		sharedDay.dir, sharedDay.err = os.MkdirTemp("", "energyreport")
		if sharedDay.err != nil {
			return
		}
		sharedDay.st, sharedDay.err = state.Open(filepath.Join(sharedDay.dir, "state.db"))
		if sharedDay.err == nil {
			record(t, sharedDay.st, day.Add(-10*time.Minute), day.Add(24*time.Hour+10*time.Minute), nil)
		}
	})
	if sharedDay.err != nil {
		t.Fatal(sharedDay.err)
	}
	return sharedDay.st
}

func wholeDay(t *testing.T, skip func(string, time.Time) bool) *state.Store {
	st := openStore(t)
	record(t, st, day.Add(-10*time.Minute), day.Add(24*time.Hour+10*time.Minute), skip)
	return st
}

func TestAWholeDayIsSixWholeKWhSums(t *testing.T) {
	st := whole(t)
	r, ok, err := Day(context.Background(), st, day.Add(5*time.Hour))
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if r.Date != "2026-10-23" || r.GridImport != 24 || r.GridExport != 0 || r.PVGeneration != 12 ||
		r.BatteryCharge != 6 || r.BatteryDischarge != 0 || r.VehicleCharge != 24 || !r.Valid() {
		t.Fatalf("%+v", r)
	}
}

func TestAPartDayIsNotSent(t *testing.T) {
	for name, skip := range map[string]func(string, time.Time) bool{
		"solar silent for an hour": func(id string, at time.Time) bool {
			return id == "inverter:1/pv" && at.After(day.Add(13*time.Hour)) && at.Before(day.Add(14*time.Hour))
		},
		"a battery that stopped at noon": func(id string, at time.Time) bool {
			return id == "battery:1/battery" && at.After(day.Add(12*time.Hour))
		},
		"a charger added at noon": func(id string, at time.Time) bool {
			return id == "charger:1/vehicle_charger" && at.Before(day.Add(12*time.Hour))
		},
		"no site meter": func(id string, _ time.Time) bool { return id == "meter:1/grid_meter" },
	} {
		t.Run(name, func(t *testing.T) {
			if r, ok, err := Day(context.Background(), wholeDay(t, skip), day); err != nil || ok {
				t.Fatalf("sent %+v (err %v)", r, err)
			}
		})
	}
	// The day after the ledger ends has not happened yet.
	if _, ok, _ := Day(context.Background(), whole(t), day.AddDate(0, 0, 1)); ok {
		t.Fatal("an unrecorded day was sent")
	}
}

// A meter read by its counters keeps a whole day across an outage: the
// counter carries the three hours' energy over the gap, and the ledger
// spreads it.
func TestACounterCarriesTheMeterAcrossAnOutage(t *testing.T) {
	only := map[string]bool{"meter:1/grid_meter": true}
	st := wholeDay(t, func(id string, at time.Time) bool {
		return !only[id] || (at.After(day.Add(3*time.Hour)) && at.Before(day.Add(6*time.Hour)))
	})
	r, ok, err := Day(context.Background(), st, day)
	if err != nil || !ok || r.GridImport != 24 || r.PVGeneration != 0 {
		t.Fatalf("%+v ok %v err %v", r, ok, err)
	}
}

// An asset that left before the day began is not part of it.
func TestAnAssetRetiredBeforeTheDayIsLeftOut(t *testing.T) {
	st := openStore(t)
	record(t, st, day.Add(-48*time.Hour), day.Add(24*time.Hour+10*time.Minute), func(id string, at time.Time) bool {
		return id == "battery:1/battery" && !at.Before(day.Add(-24*time.Hour))
	})
	r, ok, err := Day(context.Background(), st, day)
	if err != nil || !ok || r.BatteryCharge != 0 || r.GridImport != 24 {
		t.Fatalf("%+v ok %v err %v", r, ok, err)
	}
}

// An asset first seen after the day ended is not part of it.
func TestAnAssetAddedAfterTheDayLeavesItWhole(t *testing.T) {
	st := wholeDay(t, nil)
	w := 400.0
	for _, at := range []time.Time{day.Add(24*time.Hour + 30*time.Minute), day.Add(24*time.Hour + 32*time.Minute)} {
		o := state.EnergyObservation{AssetID: "battery:2/battery", DeviceID: "battery:2", AssetKind: state.AssetBattery, Label: "x",
			Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: &w}
		if err := st.RecordTickWithOptionalHistory(nil, nil, []state.EnergyObservation{o}); err != nil {
			t.Fatal(err)
		}
	}
	if r, ok, err := Day(context.Background(), st, day); err != nil || !ok || r.BatteryCharge != 6 {
		t.Fatalf("%+v ok %v err %v", r, ok, err)
	}
}

// A charger's session counter starts again at every plug-in. The ledger marks
// a reset and covers the interval from the power reading, so the day stays
// whole and the charge is counted.
func TestASessionCounterThatStartsAgainKeepsTheDay(t *testing.T) {
	st := wholeDay(t, func(id string, _ time.Time) bool { return id == "charger:1/vehicle_charger" })
	var obs []state.EnergyObservation
	first := day.Add(-10 * time.Minute)
	for at := first; !at.After(day.Add(24*time.Hour + 10*time.Minute)); at = at.Add(tick) {
		session := first
		if !at.Before(day.Add(12 * time.Hour)) {
			session = day.Add(12 * time.Hour)
		}
		w, c := 1000.0, 1000*at.Sub(session).Hours()
		obs = append(obs, state.EnergyObservation{AssetID: "charger:2/vehicle_charger", DeviceID: "charger:2", AssetKind: state.AssetVehicleCharger,
			Label: "x", Flow: state.FlowVehicleCharge, AtMs: at.UnixMilli(), PowerW: &w, CounterWh: &c})
	}
	if err := st.RecordTickWithOptionalHistory(nil, nil, obs); err != nil {
		t.Fatal(err)
	}
	r, ok, err := Day(context.Background(), st, day)
	if err != nil || !ok || r.VehicleCharge != 24 {
		t.Fatalf("%+v ok %v err %v", r, ok, err)
	}
}
