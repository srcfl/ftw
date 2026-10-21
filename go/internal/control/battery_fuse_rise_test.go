package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evSplitFixture is a 25 A site with a 1 A margin (16,560 W). It holds the
// meter reading, our recent commands and the EV demand; device readings are
// left out on purpose, since the split must not read them.
func evSplitFixture(t *testing.T, gridW float64, phaseA []float64, demandW float64, demandPhase string) (*telemetry.Store, *State) {
	t.Helper()
	store := telemetry.NewStore()
	data := map[string]any{}
	for i, key := range []string{"l1_a", "l2_a", "l3_a"} {
		if i < len(phaseA) {
			data[key] = phaseA[i]
		}
	}
	raw, _ := json.Marshal(data)
	store.Update("meter", telemetry.DerMeter, gridW, nil, raw)
	store.RecordDriverSuccess("meter")
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.DriverLimits = map[string]PowerLimits{"first": {MaxChargeW: 10000, MaxDischargeW: 5000}, "second": {MaxChargeW: 10000, MaxDischargeW: 5000}}
	state.batteryCommands = map[string]commandLog{}
	if demandW > 0 {
		state.evFuseDemands = []evFuseDemand{{"car", demandW, demandPhase}}
	}
	return store, state
}

// sent logs our commands for a device, oldest first, ending now. The first
// has been in force for longer than the settle time.
func sent(state *State, driver string, watts ...float64) {
	now := state.now()
	var log commandLog
	for i, w := range watts {
		at := now.Add(time.Duration(i-len(watts)+1) * 2 * time.Second)
		if i == 0 {
			at = now.Add(-evSettle - time.Second)
		}
		log = log.add(at, w)
	}
	if driver == "ev" {
		for i := range log {
			log[i].three = true
		}
		state.evOffers, state.EVCommandedW = log, watts[len(watts)-1]
		state.evLimit.offers = map[string]chargerOffers{"car": {driver: "car", log: log, settle: evSettle}}
		state.evCommanded = map[string]float64{"car": watts[len(watts)-1]}
		return
	}
	state.batteryCommands[driver] = log
}

func targetW(out []DispatchTarget, driver string) float64 {
	for _, target := range out {
		if target.Driver == driver {
			return target.TargetW
		}
	}
	return math.NaN()
}

// House 3 kW, our 4,140 W EV offer, a 10,350 W demand: 6,210 W of the
// remaining room is the car's before the battery may charge.
func TestEVBatteryChargeYieldsToEVNeed(t *testing.T) {
	store, state := evSplitFixture(t, 7140, nil, 10350, "3p")
	sent(state, "ev", 4140, 4140)
	sent(state, "first", 0, 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state)
	if got, want := targetW(out, "first"), 16560.0-7140-6210; math.Abs(got-want) > .01 {
		t.Fatalf("battery %.1f W, want %.1f W", got, want)
	}
}

// An EV offer raised within the settle time may not be in the meter yet;
// the lower offer is what the meter is sure to contain.
func TestEVBatteryRecentEVRiseIsStillNeed(t *testing.T) {
	store, state := evSplitFixture(t, 7140, nil, 10350, "3p")
	sent(state, "ev", 4140, 10350)
	sent(state, "first", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state)
	if got, want := targetW(out, "first"), 16560.0-7140-6210; math.Abs(got-want) > .01 {
		t.Fatalf("battery %.1f W spent the unseen EV rise, want %.1f W", got, want)
	}
}

// Our 4 kW cut on one battery may not be in the meter yet. It cannot fund
// the sibling's charge.
func TestEVBatteryReductionDoesNotFundSibling(t *testing.T) {
	store, state := evSplitFixture(t, 12000, nil, 6000, "3p")
	sent(state, "ev", 5000)
	sent(state, "first", 4000, 0)
	sent(state, "second", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 0}, {Driver: "second", TargetW: 8000}}, store, state)
	if got, want := targetW(out, "second"), 16560.0-12000-1000; got > want+.01 {
		t.Fatalf("sibling %.1f W used an unseen reduction, want <= %.1f W", got, want)
	}
}

// Stopping our 5 kW discharge raises import by 5 kW; that supply is gone
// before any battery may charge.
func TestEVBatteryStoppingDischargeReservesItsSupply(t *testing.T) {
	store, state := evSplitFixture(t, 6000, nil, 2000, "3p")
	sent(state, "ev", 0)
	sent(state, "first", -5000)
	sent(state, "second", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 0}, {Driver: "second", TargetW: 8000}}, store, state)
	if got, want := targetW(out, "second"), 16560.0-6000-2000-5000; got > want+.01 {
		t.Fatalf("sibling %.1f W, want <= %.1f W", got, want)
	}
}

// Our own 4 kW rise that the meter may not show is reserved once, not
// twice, when the same order is repeated: 9,000 + 4,000 + 2,000 W fits.
func TestEVBatteryOwnPendingRiseCountsOnce(t *testing.T) {
	for _, tc := range []struct{ target, want float64 }{{6000, 6000}, {9000, 2000 + 16560 - 9000 - 2000}} {
		store, state := evSplitFixture(t, 9000, nil, 2000, "3p")
		sent(state, "ev", 0)
		sent(state, "first", 2000, 6000)
		out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: tc.target}}, store, state)
		if got := targetW(out, "first"); math.Abs(got-tc.want) > .01 {
			t.Fatalf("order %.0f W became %.1f W, want %.1f W", tc.target, got, tc.want)
		}
	}
}

// A single-phase car may land its whole need on the worst phase.
func TestEVBatterySinglePhaseNeedUsesWorstPhase(t *testing.T) {
	store, state := evSplitFixture(t, 4600, []float64{10, 5, 5}, 3680, "1p")
	sent(state, "ev", 0)
	sent(state, "first", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 3000}}, store, state)
	if got := targetW(out, "first"); got != 0 {
		t.Fatalf("battery %.1f W; L1 10 A + 16 A car leaves no room under 24 A", got)
	}
}

// Without a recent command a battery may be discharging and stop; its
// configured discharge stays reserved.
func TestEVBatteryWithoutCommandsReservesDischarge(t *testing.T) {
	store, state := evSplitFixture(t, 5000, nil, 4140, "3p")
	sent(state, "ev", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state)
	if got, want := targetW(out, "first"), 16560.0-5000-4140-5000; got > want+.01 {
		t.Fatalf("battery %.1f W, want <= %.1f W", got, want)
	}
}

func TestEVBatteryUnknownMeterClampsOnlyPositiveOffers(t *testing.T) {
	store, state := evSplitFixture(t, 0, nil, 4140, "3p")
	store.Remove("meter")
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 3000}, {Driver: "second", TargetW: -2000}}, store, state)
	if targetW(out, "first") != 0 || targetW(out, "second") != -2000 {
		t.Fatalf("unknown meter: %+v", out)
	}
}

// Charge that brings measured export back inside its ceiling stays.
func TestEVBatteryKeepsChargeNeededForMeasuredExportRelief(t *testing.T) {
	store, state := evSplitFixture(t, -19000, nil, 4140, "3p")
	state.MaxExportW = 10000
	sent(state, "ev", 0)
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 12000}}, store, state)
	if got := targetW(out, "first"); got < 9000-.01 {
		t.Fatalf("export relief cut to %.1f W, want >= 9,000 W", got)
	}
}

// The split reads our commands and the meter only. A lone, missing, late
// or too high reading from the charger or battery changes nothing.
func TestEVBatterySplitIgnoresDeviceReadings(t *testing.T) {
	run := func(readings func(*telemetry.Store)) float64 {
		store, state := evSplitFixture(t, 7140, nil, 10350, "3p")
		sent(state, "ev", 4140, 4140)
		sent(state, "first", 0, 0)
		readings(store)
		return targetW(fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state), "first")
	}
	want := run(func(*telemetry.Store) {})
	for name, readings := range map[string]func(*telemetry.Store){
		"ev_lone_zero":   func(s *telemetry.Store) { s.Update("car", telemetry.DerEV, 0, nil, nil) },
		"ev_above_offer": func(s *telemetry.Store) { s.Update("car", telemetry.DerEV, 11000, nil, nil) },
		"ev_unavailable": func(s *telemetry.Store) {
			s.Update("car", telemetry.DerEV, 0, nil, []byte(`{"control_power_available":false}`))
		},
		"battery_discharging": func(s *telemetry.Store) { s.Update("first", telemetry.DerBattery, -5000, nil, nil) },
		"battery_charging":    func(s *telemetry.Store) { s.Update("first", telemetry.DerBattery, 6000, nil, nil) },
	} {
		if got := run(readings); got != want {
			t.Fatalf("%s moved the battery target %.1f -> %.1f W", name, want, got)
		}
	}
}
