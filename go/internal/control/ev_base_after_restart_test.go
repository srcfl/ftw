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
		{"measured draw below the offer", func(store *telemetry.Store, _ *State) {
			store.Update("charger", telemetry.DerEV, 4000, nil, nil)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
			tc.edit(store, state)
			if got := state.evFoundDrawW(store, state.now(), 1e6, 0); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("credit %.0f W, want %.0f W", got, tc.want)
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
