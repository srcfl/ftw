package control

import (
	"encoding/json"
	"maps"
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
// order, so the car still draws what it drew before the restart. Its
// max_charge_w is 11,040 W (16 A). evW is what the charger measures,
// stamped evClockAllowance before the site reading arrived; the site meter
// reads gridW with the phase currents phaseA, and the PV inverter pvW.
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
	setChargerSample(store, evW, nil)

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
		settle: evUnknownSettle, found: found, driver: "charger", maxW: 11040,
	}}
	return store, state
}

// C10 (27 Oct triage, seed 207532): after a restart, the 0 A offers sent
// while the meter was stale counted as the EV base the meter shows, while
// the meter showed the car at 8.2 kW. The new 6.2 kW order then counted as
// a 9 A rise to come, and the battery was held in discharge for the car
// with battery_covers_ev off, down toward its floor (-3,599 W here). The
// car's measured draw is in the reading: the 6.2 kW order is a cut. Until
// it lands the car may still rise to the 16 A of an offer Core does not
// know, so the battery keeps only the discharge that rise needs on L2.
func TestEVBaseAfterRestartCreditsMeasuredDraw(t *testing.T) {
	store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
	riseA := (11040 - 8222) / 690.0
	want := -(5000 - (16-4.97-riseA)*690)
	if got := targetW(out, "sungrow"); math.Abs(got-want) > 1 {
		t.Fatalf("battery %.0f W, want %.0f W: only the car's rise to its 16 A max needs room", got, want)
	}
}

// Review of #1926: a car below the offer in force. The car draws 8.2 kW,
// fresh and inside what the meter shows, but the charger's offer from
// before the restart, or its own default, is 16 A. Until Core's 6.2 kW
// order lands the car may rise to 11 kW (a ramp, a heater). The battery
// keeps the discharge that rise needs.
func TestEVBaseAfterRestartCarBelowOfferInForce(t *testing.T) {
	houseA := 3.0
	evW, batW := 8222.0, -5000.0
	gridW := houseA*690 + evW + batW
	phaseA := gridW / 690
	store, state := restartFixture(t, gridW, [3]float64{phaseA, phaseA, phaseA}, batW, 0, evW)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
	got := targetW(out, "sungrow")
	if afterA := phaseA + (got-batW)/690 + (11040-evW)/690; afterA > 16+1e-6 {
		t.Fatalf("battery ends discharge to %.0f W; a car rising to its offer in force puts %.2f A on each phase", got, afterA)
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

// chargerData is the fixture charger's data: its sample stamped
// evClockAllowance before the site reading arrived, with extra fields.
func chargerData(store *telemetry.Store, extra map[string]any) json.RawMessage {
	at := store.Get("meter", telemetry.DerMeter).UpdatedAt.Add(-evClockAllowance)
	d := map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)}
	maps.Copy(d, extra)
	raw, _ := json.Marshal(d)
	return raw
}

// setChargerSample makes w the fixture charger's reading (chargerData),
// arriving with the site reading.
func setChargerSample(store *telemetry.Store, w float64, extra map[string]any) {
	store.Update("charger", telemetry.DerEV, w, nil, chargerData(store, extra))
	store.RecordDriverSuccess("charger")
	store.Get("charger", telemetry.DerEV).UpdatedAt = store.Get("meter", telemetry.DerMeter).UpdatedAt
}

// setMaxW sets the fixture charger's max_charge_w.
func setMaxW(state *State, w float64) {
	c := state.evLimit.offers["garage"]
	c.maxW = w
	state.evLimit.offers["garage"] = c
}

// setRiseCapW puts both max_charge_w and the site fuse at w, the most an
// offer in force may be, so a case can test the other bounds alone.
func setRiseCapW(state *State, w float64) {
	setMaxW(state, w)
	state.SiteFuseAmps = w / (state.SiteFuseVoltage * float64(state.SiteFusePhases))
}

// The credit never exceeds the latest offer, keeps room for the car's rise
// to its max, takes the lowest of the sample, raw and smoothed draw and of
// every sample since the site reading, needs a live charger reading
// stamped evClockAllowance before the site reading, and ends once the
// charger's first offer of this run has had its settle time: then the
// offers alone say what the meter shows.
func TestEVFoundDrawWBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*telemetry.Store, *State)
		want float64
	}{
		{"the car may still rise to its max", func(*telemetry.Store, *State) {}, 8222 - (11040 - 6210)},
		{"no max_charge_w", func(_ *telemetry.Store, state *State) { setMaxW(state, 0) }, 0},
		{"max_charge_w below the fuse: the offer in force may still be the fuse's", func(_ *telemetry.Store, state *State) {
			setMaxW(state, 7360)
		}, 8222 - (11040 - 6210)},
		{"a charger sample measured after the site reading", func(store *telemetry.Store, state *State) {
			store.Get("meter", telemetry.DerMeter).UpdatedAt = state.now().Add(-time.Second)
			store.Get("charger", telemetry.DerEV).UpdatedAt = state.now()
			store.Get("charger", telemetry.DerEV).Data = chargerData(store, map[string]any{"control_power_observed_at": state.now().Format(time.RFC3339Nano)})
		}, 0},
		{"a sample stamped less than evClockAllowance before the site reading", func(store *telemetry.Store, _ *State) {
			at := store.Get("meter", telemetry.DerMeter).UpdatedAt.Add(-evClockAllowance + time.Second)
			store.Get("charger", telemetry.DerEV).Data = chargerData(store, map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)})
		}, 0},
		{"only a sample older than one control interval before the reading", func(store *telemetry.Store, state *State) {
			// The car may have stopped and started again since that sample;
			// the current one, 1 s before the reading, may be ahead of it.
			risenSince(store, state, 8222, 11040)
			c := state.evLimit.offers["garage"]
			c.draws[0].at = c.draws[0].at.Add(-10 * time.Second)
			c.draws[0].seen = c.draws[0].at
			state.evLimit.offers["garage"] = c
		}, 0},
		{"a sample since the site reading reads less", func(store *telemetry.Store, state *State) {
			risenSince(store, state, 8222, 6000)
		}, 6000 - (11040 - 6210)},
		// Review of #1926: OCPP stamps MeterValues with the charger's own
		// clock. One running 1 s behind Core's stamps a rise sampled after
		// the site reading as 1 s before it. The draw stays the one sampled
		// before the reading.
		{"a rise stamped before the reading by a charger clock behind Core's", func(store *telemetry.Store, state *State) {
			risenSince(store, state, 6900, 11040)
		}, 6900 - (11040 - 6210)},
		{"latest offer bounds the draw", func(_ *telemetry.Store, state *State) { setRiseCapW(state, 6210) }, 6210},
		{"measured draw below the offer", func(store *telemetry.Store, state *State) {
			setRiseCapW(state, 6210)
			setChargerSample(store, 4000, nil)
		}, 4000},
		{"smoothed draw below the sample", func(store *telemetry.Store, state *State) {
			setRiseCapW(state, 6210)
			store.Get("charger", telemetry.DerEV).SmoothedW = 5000
		}, 5000},
		{"raw draw below the sample", func(store *telemetry.Store, state *State) {
			setRiseCapW(state, 6210)
			r := store.Get("charger", telemetry.DerEV)
			r.Data = chargerData(store, map[string]any{"control_power_w": 8222})
			r.RawW = 5000
		}, 5000},
		{"control sample below raw and smoothed", func(store *telemetry.Store, state *State) {
			setRiseCapW(state, 6210)
			store.Get("charger", telemetry.DerEV).Data = chargerData(store, map[string]any{"control_power_w": 5000})
		}, 5000},
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
		}, 8222 - (11040 - 6210)},
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
			meter := store.Get("meter", telemetry.DerMeter)
			if got := state.evFoundDrawW(store, meter.RawW, meter.UpdatedAt, state.now(), shownW); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("credit %.0f W beside %.0f W shown, want %.0f W", got, shownW, tc.want)
			}
		})
	}
}

// risenSince keeps the fixture charger's sample of fromW, stamped
// evClockAllowance before the site reading, as noteDraw does, and makes
// its current reading toW, stamped 1 s before the site reading arrived.
func risenSince(store *telemetry.Store, state *State, fromW, toW float64) {
	setChargerSample(store, fromW, nil)
	c := state.evLimit.offers["garage"]
	c.noteDraw(store, state.now())
	state.evLimit.offers["garage"] = c
	at := store.Get("meter", telemetry.DerMeter).UpdatedAt.Add(-time.Second)
	setChargerSample(store, toW, map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)})
	// The filter's smoothed value trails the rise; this case reads the sample.
	store.Get("charger", telemetry.DerEV).SmoothedW = toW
}

// Offers the meter already shows come off the consumption the credit may
// fill: here another charger's 5 kW is shown, so of the 8,780 W the meter
// shows as consumption only 3,780 W is left for this car.
func TestEVFoundDrawWLeavesOutShownOffers(t *testing.T) {
	store, state := restartFixture(t, 780, [3]float64{2.03, 4.97, .47}, -5000, -3000, 8222)
	setRiseCapW(state, 6210)
	meter := store.Get("meter", telemetry.DerMeter)
	if got := state.evFoundDrawW(store, meter.RawW, meter.UpdatedAt, state.now(), 5000); math.Abs(got-3780) > 1e-6 {
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

// The consumption bound subtracts each output at the higher of raw and
// smoothed. A battery whose reading already shows a new 3 kW discharge,
// smoothed still at 1 kW, may be ahead of the site reading; so may PV.
func TestSiteConsumptionWTakesTheSmallerOutput(t *testing.T) {
	store, _ := restartFixture(t, 680, [3]float64{4.97, -1, -1}, -3000, -2000, 8222)
	store.Get("sungrow", telemetry.DerBattery).SmoothedW = -1000
	store.Get("pv", telemetry.DerPV).SmoothedW = -500
	if got := siteConsumptionW(store, 680); math.Abs(got-2180) > 1e-6 {
		t.Fatalf("consumption %.0f W, want 2180 W: 680 W less -1,000 W battery and -500 W PV", got)
	}
}

// restartSim is seed 207532's shape on the plant: Core restarts beside a
// car drawing 12 A from before, on a charger that takes 40 s to apply an
// order. For its first 6 s the meter is silent and the inverter
// unreachable, holding the 5 kW discharge it had (C1), so Core logs 0 A
// offers. Then Core asks 9 A for the car. PV covers the house total, the
// plan is stale and battery_covers_ev is off.
func restartSim(house [3]float64) *simSite {
	p := simPlant(simStart)
	p.Fuse.RatingA, p.HouseW, p.PVW = 16, house, -6000
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
	return s
}

// The seed on the plant: the battery ends its discharge as fast as its
// slew allows, down to what the car's rise to its 16 A max still needs on
// L2 (the 9 A of house load PV does not cover there), and no phase passes
// the fuse line on the way.
func TestSiteSimRestartEndsDischargeForACarAlreadyDrawing(t *testing.T) {
	s := restartSim(houseA(4, 9, 2))
	s.boot()
	s.run(14 * time.Second)
	if s.offerW != 6210 || s.p.EVSE.DrawW != 8280 {
		t.Fatalf("not the seed's state: offer %.0f W, car %.0f W", s.offerW, s.p.EVSE.DrawW)
	}
	if want := -(9*230 - 2000.0) * 3; s.batteryCmdW < want-1 {
		t.Fatalf("battery held at %.0f W 8 s after the meter returned, with the car already in the reading; want %.0f W", s.batteryCmdW, want)
	}
	s.run(2 * time.Minute)
	if s.failure != "" {
		t.Fatal(s.failure)
	}
}

// The seed's charger is OCPP and samples every 10 s (MeterValues). Its
// sample counts only on the ticks just after it, so the battery holds its
// discharge between samples. It still ends it before the 9 A order counts
// as shown (evSettle after it went out, about 28 s in), and no phase
// passes the fuse line. A charger sampling every 30 s or more gets no
// credit in that time: the fix is partial for it.
func TestSiteSimRestartTenSecondSamples(t *testing.T) {
	s := restartSim(houseA(4, 9, 2))
	s.evEvery = 10 * time.Second
	s.boot()
	s.run(20 * time.Second)
	if s.offerW != 6210 || s.p.EVSE.DrawW != 8280 {
		t.Fatalf("not the seed's state: offer %.0f W, car %.0f W", s.offerW, s.p.EVSE.DrawW)
	}
	if want := -(9*230 - 2000.0) * 3; s.batteryCmdW < want-1 {
		t.Fatalf("battery held at %.0f W 14 s after the meter returned; want %.0f W", s.batteryCmdW, want)
	}
	s.run(2 * time.Minute)
	if s.failure != "" {
		t.Fatal(s.failure)
	}
}

// Review of #1926 on the plant: the charger's offer in force is 16 A, and
// the car limits itself to 12 A at the restart, then lifts that 14 s
// later, before the 40 s charger has applied Core's order. L2 carries 12 A
// of house. The battery keeps the discharge the car's rise needs: no phase
// reaches 1.2 x In, and time over In stays within the heat model.
func TestSiteSimRestartCarRisesUnderOfferInForce(t *testing.T) {
	s := restartSim(houseA(4, 12, 2))
	carRisesUnderOldOffer(t, s, 8280, 14*time.Second)
}

// carRisesUnderOldOffer runs s with the charger still on the 16 A offer it
// had before the restart, and a car that draws fromW until it lifts its own
// limit at riseAt. No phase may reach 1.2 x In, and the sim's fuse checks
// must pass.
func carRisesUnderOldOffer(t *testing.T, s *simSite, fromW float64, riseAt time.Duration) {
	t.Helper()
	s.p.EVSE.OfferA, s.p.Car.MaxW = 16, fromW
	s.p.Events = append(s.p.Events, sitesim.Event{At: riseAt, Kind: sitesim.Change, Do: func(p *sitesim.Plant) { p.Car.MaxW = 11040 }})
	s.boot()
	s.run(2 * time.Minute)
	if s.failure != "" {
		t.Fatal(s.failure)
	}
	if s.peakA >= fuseInstantFactor*16 {
		t.Fatalf("a phase reached %.2f A", s.peakA)
	}
}

// Third review of #1926: a site meter that samples every 3 s. The car draws
// 10 A under the 16 A offer in force and lifts its own limit 11 s in. The
// charger reads the rise before the next meter sample, so its sample is
// newer than the site reading, which still shows the old draw. That sample
// does not count as in the reading, and the battery keeps the discharge
// the rise needs. Before: L2 reached 19.30 A.
func TestSiteSimRestartCarRisesBetweenMeterSamples(t *testing.T) {
	s := restartSim(houseA(4, 12, 2))
	s.p.Meter.Every = 3 * time.Second
	carRisesUnderOldOffer(t, s, 6900, 11*time.Second)
}

// Third review of #1926: max_charge_w is not the most an offer in force
// may be. The owner lowered it to 7,360 W (10.67 A) and restarted; the
// charger still runs the 16 A Core sent before. The car draws 7 kW and may
// rise to 11,040 W before the 6.2 kW order lands. The rise reserve runs to
// the site fuse, so the battery keeps the discharge that rise needs.
func TestEVBaseAfterRestartMaxChargeLoweredAtRestart(t *testing.T) {
	house := 3.0
	evW, batW := 7000.0, -5000.0
	gridW := house*690 + evW + batW
	phaseA := gridW / 690
	store, state := restartFixture(t, gridW, [3]float64{phaseA, phaseA, phaseA}, batW, 0, evW)
	setMaxW(state, 7360)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "sungrow", TargetW: 0}}, store, state, 16*230*3)
	got := targetW(out, "sungrow")
	if afterA := phaseA + (got-batW)/690 + (11040-evW)/690; afterA > 16+1e-6 {
		t.Fatalf("battery ends discharge to %.0f W; the car rising to the old 16 A offer puts %.2f A on each phase", got, afterA)
	}
}

// The plant form: max_charge_w is now 8,280 W (12 A), and the car lifts
// its own 12 A limit 14 s in, before the 40 s charger applies Core's order.
// Before: L2 reached 19.30 A.
func TestSiteSimRestartMaxChargeLoweredAtRestart(t *testing.T) {
	s := restartSim(houseA(4, 12, 2))
	s.lp.MaxChargeW, s.lp.AllowedStepsW = 8280, []float64{0}
	for a := 6.0; a <= 12; a++ {
		s.lp.AllowedStepsW = append(s.lp.AllowedStepsW, a*690)
	}
	carRisesUnderOldOffer(t, s, 8280, 14*time.Second)
}

// Review of #1926: OCPP stamps MeterValues with the charger's own clock,
// often cut to the whole second. With the charger's clock 1-3 s behind
// Core's, the third review's case (3 s meter, the car at 10 A under a 16 A
// offer in force lifting its own limit 11 s in) stamps the rise before the
// site reading that does not show it yet. The draw stays the one sampled
// before that reading. Before: L2 reached 19.30 A.
func TestSiteSimRestartChargerClockBehind(t *testing.T) {
	for _, lag := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} {
		t.Run(lag.String(), func(t *testing.T) {
			s := restartSim(houseA(4, 12, 2))
			s.p.Meter.Every = 3 * time.Second
			s.evClockLag = lag
			carRisesUnderOldOffer(t, s, 6900, 11*time.Second)
		})
	}
}

// Review of #1926: a 22 kW charger (32 A) on a 3 x 25 A main fuse. The
// offer in force may be the charger's 32 A, so the rise reserve runs to
// max_charge_w, not to the fuse's rating: of the 14 kW the car draws,
// 22,080 - 11,000 W may still come on top of Core's latest 11 kW offer.
func TestEVFoundDrawWMaxChargeAboveFuse(t *testing.T) {
	evW := 14000.0
	store, state := restartFixture(t, 9000, [3]float64{13, 13, 13}, -5000, 0, evW)
	state.SiteFuseAmps = 25
	setMaxW(state, 22080)
	c := state.evLimit.offers["garage"]
	c.log[len(c.log)-1].w = 11000
	state.evLimit.offers["garage"] = c
	meter := store.Get("meter", telemetry.DerMeter)
	if got, want := state.evFoundDrawW(store, meter.RawW, meter.UpdatedAt, state.now(), 0), evW-(22080-11000); math.Abs(got-want) > 1e-6 {
		t.Fatalf("credit %.0f W, want %.0f W", got, want)
	}
}
