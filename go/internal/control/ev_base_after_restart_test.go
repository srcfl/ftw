package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// restartFixture is seed 207532 just after Core restarted: a 16 A site with
// no margin, a battery FTW found discharging 5 kW, and a charger whose first
// logged offers of this run are 0 A, sent while the meter was stale, then
// 6,210 W (9 A on three phases) 2 s ago. The charger takes 40 s to apply an
// order, so the car still draws what it drew before the restart. evW is
// what the charger measures; the site meter reads gridW with the phase
// currents phaseA, and the PV inverter pvW.
func restartFixture(t *testing.T, gridW float64, phaseA [3]float64, batteryW, pvW, evW float64) (*telemetry.Store, *State) {
	t.Helper()
	store := telemetry.NewStore()
	raw, _ := json.Marshal(map[string]any{"l1_a": phaseA[0], "l2_a": phaseA[1], "l3_a": phaseA[2]})
	store.Update("meter", telemetry.DerMeter, gridW, nil, raw)
	store.RecordDriverSuccess("meter")
	store.Update("sungrow", telemetry.DerBattery, batteryW, ptrF64(.3), nil)
	store.RecordDriverSuccess("sungrow")
	if pvW != 0 {
		store.Update("pv", telemetry.DerPV, pvW, nil, nil)
		store.RecordDriverSuccess("pv")
	}
	store.Update("charger", telemetry.DerEV, evW, nil, nil)
	store.RecordDriverSuccess("charger")

	state := NewState(0, 0, "meter")
	now := time.Now()
	state.clock = func() time.Time { return now }
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 16, 230, 3, 0
	state.DriverLimits = map[string]PowerLimits{"sungrow": {MaxChargeW: 5000, MaxDischargeW: 5000}}
	state.batteryCommands = map[string]commandLog{"sungrow": commandLog{}.add(now.Add(-2*time.Second), batteryW)}
	state.evFuseDemands = []evFuseDemand{{"garage", 6210, "3p"}}
	state.EVCommandedW = 6210
	found := sentCommand{at: now.Add(-6 * time.Second), w: 0, a: 0, n: 3}
	state.evLimit.set = true
	state.evLimit.offers = map[string]chargerOffers{"garage": {
		log:    commandLog{found, {at: now.Add(-4 * time.Second), w: 0, a: 0, n: 3}, {at: now.Add(-2 * time.Second), w: 6210, a: 9, n: 3}},
		settle: evUnknownSettle, found: found, driver: "charger",
	}}
	return store, state
}

// C10 (27 Oct triage, seed 207532): after a restart, the 0 A offers sent
// while the meter was stale counted as the EV base the meter shows, while
// the meter showed the car at 8.2 kW. The new 6.2 kW order then counted as
// a 9 A rise to come, and the battery was held in discharge for the car
// with battery_covers_ev off, down toward its floor. The car's measured
// draw is in the reading: the 6.2 kW order is a cut, and ending the
// discharge needs no room for it.
func TestEVBaseAfterRestartCreditsMeasuredDraw(t *testing.T) {
	store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
	if got := targetW(out, "sungrow"); got < -1 {
		t.Fatalf("battery held at %.0f W for a car the meter already shows at 8.2 kW; want 0 W", got)
	}
}

// The credit stays on the safe side. A charger reading the meter cannot
// contain counts only up to the consumption the meter shows: here the
// charger claims 8.2 kW, but the site consumes 680 W, so 5,530 W of the
// 6,210 W order is still a rise to come, and the battery keeps the
// discharge that the busiest phase has no room to end.
func TestEVBaseAfterRestartCreditStaysInsideTheMeter(t *testing.T) {
	store, state := restartFixture(t, 680, [3]float64{4.97, -1, -1}, 0, 0, 8222)
	state.batteryCommands["sungrow"] = commandLog{}.add(state.now().Add(-time.Second), -5000)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
	riseA := (6210 - 680) / 690.0
	want := -(5000 - (16-4.97-riseA)*690)
	if got := targetW(out, "sungrow"); math.Abs(got-want) > 1 {
		t.Fatalf("battery %.0f W, want %.0f W: the charger's reading counts only up to the meter's 680 W", got, want)
	}
}

// The credit never exceeds the latest offer, needs a live charger reading,
// and ends once the charger's first offer of this run has had its settle
// time: then the offers alone say what the meter shows.
func TestEVFoundDrawWBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*telemetry.Store, *State)
		want float64
	}{
		{"latest offer bounds the draw", func(*telemetry.Store, *State) {}, 6210},
		{"measured draw below the offer", func(store *telemetry.Store, state *State) {
			store.Update("charger", telemetry.DerEV, 4000, nil, nil)
			store.Get("charger", telemetry.DerEV).UpdatedAt = state.now()
		}, 4000},
		{"charger offline", func(store *telemetry.Store, _ *State) {
			store.DriverHealthMut("charger").SetOffline()
		}, 0},
		{"no restart", func(_ *telemetry.Store, state *State) {
			c := state.evLimit.offers["garage"]
			c.found = sentCommand{}
			state.evLimit.offers["garage"] = c
		}, 0},
		{"first offer settled", func(_ *telemetry.Store, state *State) {
			c := state.evLimit.offers["garage"]
			c.found.at = state.now().Add(-c.settle)
			state.evLimit.offers["garage"] = c
		}, 0},
		{"a 40 s charger's window outlasts evSettle", func(_ *telemetry.Store, state *State) {
			c := state.evLimit.offers["garage"]
			c.settle, c.declared = 40*time.Second, true
			c.found.at = state.now().Add(-30 * time.Second)
			c.log[0].at = c.found.at
			state.evLimit.offers["garage"] = c
		}, 6210},
		{"the shown part of the latest offer is not counted twice", func(_ *telemetry.Store, state *State) {
			c := state.evLimit.offers["garage"]
			c.found.at = state.now().Add(-60 * time.Second)
			c.log = commandLog{c.found, {at: state.now().Add(-25 * time.Second), w: 6210, a: 9, n: 3}}
			state.evLimit.offers["garage"] = c
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
			tc.edit(store, state)
			shownW := state.evShownW(state.now())
			if got := state.evFoundDrawW(store, store.Get("meter", telemetry.DerMeter), state.now(), shownW); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("credit %.0f W beside %.0f W shown, want %.0f W", got, shownW, tc.want)
			}
		})
	}
}

// Offers the meter already shows come off the consumption the credit may
// fill: here another charger's 5 kW is shown, so of the 8,780 W the meter
// shows as consumption only 3,780 W is left for this car.
func TestEVFoundDrawWLeavesOutShownOffers(t *testing.T) {
	store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
	if got := state.evFoundDrawW(store, store.Get("meter", telemetry.DerMeter), state.now(), 5000); math.Abs(got-3780) > 1e-6 {
		t.Fatalf("credit %.0f W, want 3780 W", got)
	}
}

// A charger reading the meter may not show gives no credit. A fast charger
// applied the 0 A sent while the meter was silent, so the car draws
// nothing, but its reading still shows the 8.2 kW from before: held after a
// failed poll, or a sample older than the site reading (an OCPP
// MeterValues interval). The house draws 7 kW on balanced phases, the
// battery runs the -5 kW discharge Core ordered in this run, and the meter
// shows 2.9 A per phase. The 9 A order still to come leaves room to end
// only (16 - 2.9 - 9) × 690 W of the discharge.
func TestEVBaseAfterRestartNeedsACurrentChargerSample(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*telemetry.Store, *State)
	}{
		{"last poll failed", func(store *telemetry.Store, _ *State) {
			store.RecordDriverError("charger", "timeout")
		}},
		{"sample older than the site reading", func(store *telemetry.Store, state *State) {
			data, _ := json.Marshal(map[string]any{"power_observed_at": state.now().Add(-10 * time.Second).Format(time.RFC3339Nano)})
			store.Update("charger", telemetry.DerEV, 8222, nil, data)
			store.Get("charger", telemetry.DerEV).UpdatedAt = state.now()
			store.RecordDriverSuccess("charger")
		}},
		{"site reading older than the sample", func(store *telemetry.Store, state *State) {
			store.Get("meter", telemetry.DerMeter).UpdatedAt = state.now().Add(-10 * time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := restartFixture(t, 2000, [3]float64{2.9, 2.9, 2.9}, -5000, 0, 8222)
			tc.edit(store, state)
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
			got := targetW(out, "sungrow")
			if afterA := 2.9 + (got+5000)/690 + 9; afterA > 16+1e-6 {
				t.Fatalf("battery ends its discharge to %.0f W on a charger reading the meter may not show: %.2f A per phase once the car's order lands", got, afterA)
			}
		})
	}
}

// The consumption bound reads the devices as the meter does. A battery
// that just ended a discharge, or PV that just dropped, still shows its old
// output in the smoothed value; counting that would raise the consumption
// the meter shows, and the credit with it, by the lag.
func TestEVBaseAfterRestartConsumptionReadsCurrentOutput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pvW          float64
		driver       string
		kind         telemetry.DerType
		consumptionW float64
	}{
		{"battery ended its discharge", 0, "sungrow", telemetry.DerBattery, 680},
		{"PV dropped", -500, "pv", telemetry.DerPV, 1180},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := restartFixture(t, 680, [3]float64{4.97, -1, -1}, 0, tc.pvW, 8222)
			store.Get(tc.driver, tc.kind).SmoothedW = -3000
			state.batteryCommands["sungrow"] = commandLog{}.add(state.now().Add(-time.Second), -5000)
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
			riseA := (6210 - tc.consumptionW) / 690
			want := -(5000 - (16-4.97-riseA)*690)
			if got := targetW(out, "sungrow"); math.Abs(got-want) > 1 {
				t.Fatalf("battery %.0f W, want %.0f W: the meter shows %.0f W of consumption", got, want, tc.consumptionW)
			}
		})
	}
}

// Seed 207532's shape on the plant: Core restarts beside a car drawing
// 12 A from before, on a charger that takes 40 s to apply an order. For
// its first 6 s the meter is silent and the inverter unreachable, holding
// the 5 kW discharge it had (C1), so Core logs 0 A offers. Then Core asks
// 9 A for the car. PV covers the house, the plan is stale and
// battery_covers_ev is off: the battery ends its discharge as fast as its
// slew allows, and no phase passes the fuse line on the way.
func TestSiteSimRestartEndsDischargeForACarAlreadyDrawing(t *testing.T) {
	p := simPlant(simStart)
	p.Fuse.RatingA, p.HouseW, p.PVW = 16, houseA(4, 9, 2), -6000
	p.EVSE.Delay, p.EVSE.ReadbackLag = 40*time.Second, 0
	p.EVSE.OfferA, p.EVSE.OfferPhases = 12, 3
	p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 10000, SoC: .3, Latency: 2 * time.Second, W: -5000}
	p.Events = []sitesim.Event{
		{Kind: sitesim.MeterSilent, For: 6 * time.Second},
		{Kind: sitesim.DriverOffline, Device: "battery", For: 6 * time.Second},
	}
	s := &simSite{p: p, lp: simLoadpoint()}
	s.setup = func(s *simSite) {
		s.state.Mode = ModePlannerSelf
		s.state.SiteFuseSafetyA = 0
		s.ctrl.SetManualHold(s.lp.ID, loadpoint.ManualHold{PowerW: 6210, PhaseMode: "3p", Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
	}
	s.boot()
	s.run(14 * time.Second)
	if s.offerW != 6210 || s.p.EVSE.DrawW != 8280 {
		t.Fatalf("not the seed's state: offer %.0f W, car %.0f W", s.offerW, s.p.EVSE.DrawW)
	}
	if s.batteryCmdW < -1 {
		t.Fatalf("battery held at %.0f W 8 s after the meter returned, with the car already in the reading", s.batteryCmdW)
	}
	s.run(2 * time.Minute)
	if s.failure != "" {
		t.Fatal(s.failure)
	}
}
