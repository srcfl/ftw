package control

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestJoinedEVWaitsForMeasuredBatteryBackoffThenResumes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	const houseW, desiredEVW = 5000.0, 10350.0
	actualBattery, actualEV := 8000.0, 0.0
	config := loadpoint.Config{ID: "garage", DriverName: "charger", MinChargeW: 4140, MaxChargeW: desiredEVW,
		PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{config})
	store := telemetry.NewStore()
	dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 2500, Strategy: "arbitrage"}
	st := newStateWithEnergyDispatch(dir, "meter")
	st.clock = func() time.Time { return now }
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
	st.SlewEnabled = false
	st.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	var sampledPhaseA float64
	controller := loadpoint.NewController(mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: actualEV, PowerAt: now, SessionWhUnavailable: true}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				actualEV = cmd.PowerW
				// The physical battery may ignore a reduction. Check this at
				// the EV send, before a later battery command can conceal it.
				if phase := (houseW + actualBattery + actualEV) / 690; phase > 24.001 {
					t.Fatalf("EV spent unmeasured battery relief: %.1f A", phase)
				}
			}
			return nil
		})
	controller.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	controller.SetManualHold(config.ID, loadpoint.ManualHold{PowerW: desiredEVW, PhaseMode: "3p", Persistent: true})
	controller.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(store, st, mgr.States(), controller.FuseConfigs(), 17250) })
	controller.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(st) })
	emit := func(driver string, kind telemetry.DerType, watts float64) {
		d := map[string]any{"control_power_available": true, "control_power_observed_at": now.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			d["l1_a"], d["l2_a"], d["l3_a"] = sampledPhaseA, sampledPhaseA, sampledPhaseA
		}
		if kind == telemetry.DerEV {
			d["power_max_age_s"] = 5 // A local charger that answers within seconds.
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.Get(driver, kind).UpdatedAt = now
		store.RecordDriverSuccess(driver)
	}
	var obeyedAt, resumedAt time.Time
	for tick := 0; tick < 30; tick++ {
		now = now.Add(5 * time.Second)
		if tick == 3 {
			actualBattery, obeyedAt = 0, now // Only now does the battery obey.
		}
		sampledPhaseA = (houseW + actualBattery + actualEV) / 690
		emit("pixii", telemetry.DerBattery, actualBattery)
		emit(config.DriverName, telemetry.DerEV, actualEV)
		emit("meter", telemetry.DerMeter, houseW+actualBattery+actualEV)
		controller.TickWithDispatch(context.Background(), now, true)
		st.SetEVCommanded(mgr.States())
		st.SetEVFuseDemands(controller.FuseDemands())
		targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15000}), 17250)
		if len(targets) != 1 || targets[0].TargetW > 1210.001 {
			t.Fatalf("tick %d: battery did not yield to the EV: %+v", tick, targets)
		}
		if actualEV > 0 && resumedAt.IsZero() {
			resumedAt = now
		}
		if !obeyedAt.IsZero() {
			actualBattery = math.Max(0, targets[0].TargetW)
		}
		if (houseW+actualBattery+actualEV)/690 > 24.001 {
			t.Fatalf("tick %d: battery took the EV's headroom: EV %.0f battery %.0f W", tick, actualEV, actualBattery)
		}
	}
	if resumedAt.IsZero() || resumedAt.Sub(obeyedAt) < evRestartDwell {
		t.Fatalf("EV resumed at %v, battery obeyed at %v: want a %v dwell on measured room", resumedAt, obeyedAt, evRestartDwell)
	}
	if actualEV < 9660 {
		t.Fatalf("EV stuck at %.0f W after recovery", actualEV)
	}
}

// Whatever the EV guard may still allow, our battery orders keep the total
// inside 24 A, in either order of arrival.
func TestEVBatteryCommandOrdersStayWithinMeasuredHeadroom(t *testing.T) {
	rng := rand.New(rand.NewSource(1667))
	for sample := 0; sample < 300; sample++ {
		house := rng.Float64() * 5000
		first, second := rng.Float64()*4000, rng.Float64()*4000
		ev := rng.Float64() * 3000
		grid := house + first + second + ev
		requestedEV := rng.Float64() * 11040
		actualEV := math.Max(ev, math.Min(requestedEV, ev+16560-grid))
		amps := grid / 690
		store, state := evSplitFixture(t, grid, []float64{amps, amps, amps}, requestedEV, "3p")
		sent(state, "ev", ev)
		sent(state, "first", first)
		sent(state, "second", second)
		out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: rng.Float64() * 10000}, {Driver: "second", TargetW: rng.Float64() * 10000}}, store, state)
		for _, order := range [][2]int{{0, 1}, {1, 0}} {
			batteries := [2]float64{first, second}
			for _, i := range order {
				// A battery may ignore a reduction while its sibling obeys.
				batteries[i] = math.Max(batteries[i], out[i].TargetW)
				if total := house + actualEV + batteries[0] + batteries[1]; total > 16560.001 {
					t.Fatalf("sample %d order %v: grid %.1f W exceeds 24 A; before %.1f W targets %+v", sample, order, total, grid, out)
				}
			}
		}
	}
}

// Battery orders the meter may not show yet, the EV cap and new battery
// orders share one physical budget.
func TestEVBatteryPendingAndNewOrdersShareOnePhysicalBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(1668))
	for sample := 0; sample < 300; sample++ {
		house, first, second, ev := rng.Float64()*3000, rng.Float64()*2000, rng.Float64()*2000, rng.Float64()*2000
		grid := house + first + second + ev
		pendingFirst := first + rng.Float64()*(16560-grid)
		pendingSecond := second + rng.Float64()*(16560-grid-(pendingFirst-first))
		requestedEV := rng.Float64() * 11040
		amps := grid / 690
		store, state := evSplitFixture(t, grid, []float64{amps, amps, amps}, requestedEV, "3p")
		sent(state, "ev", ev)
		sent(state, "first", first, pendingFirst)
		sent(state, "second", second, pendingSecond)
		points := []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true, CommandedKnown: true, CommandedW: ev}}
		capW, _ := EVFuseBudget(store, state, points, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
		actualEV := math.Max(ev, math.Min(requestedEV, capW))
		out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "first", TargetW: rng.Float64() * 10000}, {Driver: "second", TargetW: rng.Float64() * 10000}}, store, state)
		worst := house + actualEV + math.Max(pendingFirst, out[0].TargetW) + math.Max(pendingSecond, out[1].TargetW)
		if worst > 16560.001 {
			t.Fatalf("sample %d: old/new battery orders and EV reach %.1f W; pending %.1f/%.1f EV %.1f targets %+v", sample, worst, pendingFirst, pendingSecond, actualEV, out)
		}
	}
}

// evBatterySite runs the EV guard and battery dispatch together, as main.go
// does: charger tick first, then battery dispatch, then the next reading.
type evBatterySite struct {
	t                *testing.T
	now              time.Time
	store            *telemetry.Store
	state            *State
	mgr              *loadpoint.Manager
	ctrl             *loadpoint.Controller
	houseW, batteryW float64
	evW              float64
	commands, offers []evTimed
	maxGridW         float64
	chargerDelay     time.Duration
	carMaxW          float64       // 0: the car takes its whole offer
	batteryDelay     time.Duration // 0: the battery follows within a tick
	batteryCommands  []evTimed
	meterW, targetW  []evTimed // every site reading and battery target
}

func newEVBatterySite(t *testing.T, houseW float64) *evBatterySite {
	t.Helper()
	return newEVBatterySiteAt(t, houseW, time.Now().Truncate(time.Second))
}

func newEVBatterySiteAt(t *testing.T, houseW float64, start time.Time) *evBatterySite {
	t.Helper()
	s := &evBatterySite{t: t, now: start, store: telemetry.NewStore(), houseW: houseW, chargerDelay: 5 * time.Second}
	config := loadpoint.Config{ID: "garage", DriverName: "charger", MinChargeW: 4140, MaxChargeW: 10350,
		PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	s.mgr = loadpoint.NewManager()
	s.mgr.Load([]loadpoint.Config{config})
	dir := SlotDirective{SlotStart: s.now, SlotEnd: s.now.Add(time.Hour), BatteryEnergyWh: 10000, Strategy: "arbitrage"}
	s.state = newStateWithEnergyDispatch(dir, "meter")
	s.state.clock = func() time.Time { return s.now }
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases, s.state.SiteFuseSafetyA = 25, 230, 3, 1
	s.state.SlewEnabled = false
	s.state.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	s.ctrl = loadpoint.NewController(s.mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: s.evW, PowerAt: s.now, SessionWhUnavailable: true}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				s.offers = append(s.offers, evTimed{s.now, cmd.PowerW})
				s.commands = append(s.commands, evTimed{s.now.Add(s.chargerDelay), cmd.PowerW})
			}
			return nil
		})
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetManualHold(config.ID, loadpoint.ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		return EVFuseBudget(s.store, s.state, s.mgr.States(), s.ctrl.FuseConfigs(), 17250)
	})
	s.ctrl.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(s.state) })
	return s
}

func (s *evBatterySite) gridW() float64 { return s.houseW + s.batteryW + s.evW }

func (s *evBatterySite) emit(driver string, kind telemetry.DerType, watts float64) {
	d := map[string]any{"control_power_available": true, "control_power_observed_at": s.now.Format(time.RFC3339Nano)}
	if kind == telemetry.DerMeter {
		a := watts / 690
		d["l1_a"], d["l2_a"], d["l3_a"] = a, a, a
	}
	data, _ := json.Marshal(d)
	soc := .5
	s.store.Update(driver, kind, watts, &soc, data)
	s.store.Get(driver, kind).UpdatedAt = s.now
	s.store.RecordDriverSuccess(driver)
}

func (s *evBatterySite) step() {
	s.now = s.now.Add(time.Second)
	for len(s.commands) > 0 && !s.commands[0].at.After(s.now) {
		s.evW, s.commands = s.commands[0].w, s.commands[1:]
		if s.carMaxW > 0 {
			s.evW = math.Min(s.evW, s.carMaxW)
		}
	}
	for len(s.batteryCommands) > 0 && !s.batteryCommands[0].at.After(s.now) {
		s.batteryW, s.batteryCommands = s.batteryCommands[0].w, s.batteryCommands[1:]
	}
	s.maxGridW = math.Max(s.maxGridW, s.gridW())
	s.meterW = append(s.meterW, evTimed{s.now, s.gridW()})
	s.emit("pixii", telemetry.DerBattery, s.batteryW)
	s.emit("charger", telemetry.DerEV, s.evW)
	s.emit("meter", telemetry.DerMeter, s.gridW())
	s.state.ObserveSiteImport(s.store)
	s.ctrl.TickWithDispatch(context.Background(), s.now, true)
	s.state.SetEVCommanded(s.mgr.States())
	s.state.SetEVFuseDemands(s.ctrl.FuseDemands())
	targets := ComputeDispatch(s.store, s.state, caps(map[string]float64{"pixii": 15000}), 17250)
	if len(targets) != 1 {
		s.t.Fatalf("targets %+v", targets)
	}
	s.targetW = append(s.targetW, evTimed{s.now, targets[0].TargetW})
	if s.batteryDelay <= 0 {
		s.batteryW = targets[0].TargetW // A local battery follows within a tick.
		return
	}
	s.batteryCommands = append(s.batteryCommands, evTimed{s.now.Add(s.batteryDelay), targets[0].TargetW})
}

func (s *evBatterySite) run(d time.Duration) {
	for end := s.now.Add(d); s.now.Before(end); {
		s.step()
	}
}

func (s *evBatterySite) offer() float64 {
	if len(s.offers) == 0 {
		return 0
	}
	return s.offers[len(s.offers)-1].w
}

func (s *evBatterySite) hourLimitW() float64 {
	limitW, _, _ := s.state.importHourLimitsW()
	return limitW
}

func since(log []evTimed, at time.Time) []evTimed {
	for i, e := range log {
		if !e.at.Before(at) {
			return log[i:]
		}
	}
	return nil
}

// A 2 kW house step lands while the battery and the car both charge. EV
// first: the car must not stop and the battery must yield. The battery
// answers within a tick, inside the fuse's heat budget, so the car keeps its
// offer; a car that is cut must get back to it within a minute.
func TestEVFirstHouseStepWhileBatteryAndEVCharge(t *testing.T) {
	s := newEVBatterySite(t, 3000)
	s.run(3*time.Minute + 30*time.Second)
	low, high := math.Inf(1), math.Inf(-1)
	for range 30 {
		s.step()
		low, high = math.Min(low, s.batteryW), math.Max(high, s.batteryW)
	}
	if high-low > 1 {
		t.Fatalf("battery hunts under a steady load: %.0f..%.0f W", low, high)
	}
	evBefore, batteryBefore := s.offer(), s.batteryW
	if evBefore != 10350 || batteryBefore <= 0 {
		t.Fatalf("setup: EV %.0f W battery %.0f W", evBefore, batteryBefore)
	}
	stepAt := s.now
	s.houseW += 2000
	var lowest float64 = evBefore
	var backAt time.Time
	for s.now.Sub(stepAt) < 3*time.Minute {
		s.step()
		lowest = math.Min(lowest, s.offer())
		if backAt.IsZero() && lowest < evBefore && s.offer() == evBefore {
			backAt = s.now
		}
	}
	back := "never cut"
	if !backAt.IsZero() {
		back = "back after " + backAt.Sub(stepAt).String()
	}
	t.Logf("EV %.0f -> lowest %.0f W, %s; battery %.0f -> %.0f W", evBefore, lowest, back, batteryBefore, s.batteryW)
	if lowest <= 0 {
		t.Fatal("the house step stopped the car")
	}
	if s.batteryW > batteryBefore-2000+.01 {
		t.Fatalf("battery %.0f -> %.0f W did not yield the step", batteryBefore, s.batteryW)
	}
	if lowest < evBefore && (backAt.IsZero() || backAt.Sub(stepAt) > time.Minute) {
		t.Fatalf("EV not back at %.0f W within a minute (now %.0f W)", evBefore, s.offer())
	}
	if s.gridW() > 16560.01 {
		t.Fatalf("grid %.0f W above 24 A after the step", s.gridW())
	}
}

// newIdleBatteryEVSite starts at a clock hour with the battery planned idle,
// so only the fuse guard moves it.
func newIdleBatteryEVSite(t *testing.T, houseW, ceilingW float64) *evBatterySite {
	t.Helper()
	start := time.Date(2026, 10, 17, 16, 0, 0, 0, time.Local)
	s := newEVBatterySiteAt(t, houseW, start)
	s.state.PeakImportCeilingW = ceilingW
	idle := SlotDirective{SlotStart: start, SlotEnd: start.Add(time.Hour), Strategy: "arbitrage"}
	s.state.SlotDirective = func(time.Time) (SlotDirective, bool) { return idle, true }
	return s
}

// A sunny afternoon under a 7.5 kW import ceiling: PV covers the house and
// 3 kW of the car, so the car draws 9,660 W, more than the ceiling.
func newCeilingEVSite(t *testing.T) *evBatterySite {
	t.Helper()
	s := newIdleBatteryEVSite(t, -3000, 7500)
	s.run(5 * time.Minute)
	if s.offer() != 9660 || s.evW != 9660 || s.batteryW != 0 {
		t.Fatalf("setup: offer %.0f W draw %.0f W battery %.0f W", s.offer(), s.evW, s.batteryW)
	}
	return s
}

// #1674, Göran (segran2: Easee Cloud, Pixii, 25 A), 17 Oct 2026 14:22:38
// UTC. An overage of the import ceiling stopped the car, and in the same
// tick the battery's fuse guard discharged for the same watts. When the stop
// reached the meter the battery was still discharging, and the site exported
// about 4 kW for ~10 s. The hour's budget waits for the stop we commanded,
// so the battery leaves those watts alone and nothing swings into export.
func TestCeilingOverageLeftToCommandedEVCutDoesNotExport(t *testing.T) {
	s := newCeilingEVSite(t)
	s.chargerDelay, s.batteryDelay = 10*time.Second, 3*time.Second
	stepAt := s.now
	s.houseW += 7000 // The sun goes and the sauna starts: 13,660 W.
	s.run(40 * time.Second)
	lowestMeter, lowestBattery := math.Inf(1), math.Inf(1)
	for _, m := range since(s.meterW, stepAt) {
		lowestMeter = math.Min(lowestMeter, m.w)
	}
	for _, b := range since(s.targetW, stepAt) {
		lowestBattery = math.Min(lowestBattery, b.w)
	}
	t.Logf("offer %.0f W; lowest meter %.0f W, lowest battery target %.0f W", s.offer(), lowestMeter, lowestBattery)
	if s.offer() != 0 || s.evW != 0 {
		t.Fatalf("the car did not stop: offer %.0f W draw %.0f W", s.offer(), s.evW)
	}
	if lowestMeter < 0 {
		t.Fatalf("the site exported %.0f W when the car's stop landed", -lowestMeter)
	}
	if lowestBattery < 0 {
		t.Fatalf("the battery discharged %.0f W for watts the car's stop removes", -lowestBattery)
	}
	if s.maxGridW > 16560 {
		t.Fatalf("grid %.0f W over the fuse while waiting for the car", s.maxGridW)
	}
	// The stop went out on the first reading over the ceiling.
	for _, m := range since(s.meterW, stepAt.Add(time.Second+s.chargerDelay)) {
		if m.w > 7500 {
			t.Fatalf("%v: meter %.0f W after the car's stop landed", m.at.Sub(stepAt), m.w)
		}
	}
}

// A cut may hold the hour's overage for evSettle and no longer. A charger
// that has not followed by then leaves the overage to the battery until the
// cut shows.
func TestCeilingOverageGetsBatteryReliefWhenEVCutIsLate(t *testing.T) {
	s := newCeilingEVSite(t)
	s.chargerDelay = time.Minute
	stepAt := s.now
	s.houseW += 7000
	s.run(time.Minute) // The stop lands one second later.
	reliefBy := stepAt.Add(time.Second + evSettle)
	t.Logf("battery %.0f W from %v; meter %.0f W", since(s.targetW, reliefBy)[0].w, reliefBy.Sub(stepAt), s.gridW())
	for _, b := range since(s.targetW, reliefBy) {
		if b.w >= 0 {
			t.Fatalf("%v: no battery relief %v after the stop was sent (target %.0f W)", b.at.Sub(stepAt), b.at.Sub(stepAt.Add(time.Second)), b.w)
		}
	}
	for _, m := range since(s.meterW, reliefBy.Add(time.Second)) {
		if m.w > s.hourLimitW()+50 {
			t.Fatalf("%v: meter %.0f W, the battery did not hold the hour's limit of %.0f W", m.at.Sub(stepAt), m.w, s.hourLimitW())
		}
	}
}

// A cut that has reached the meter removes nothing more. A new overage
// after it gets battery relief as soon as the smoothed reading shows it.
func TestCeilingOverageAfterTheCutShowsGetsBatteryRelief(t *testing.T) {
	s := newCeilingEVSite(t)
	s.houseW += 7000
	s.run(15 * time.Second) // The stop landed after 5 s.
	if s.evW != 0 || s.gridW() > 7500 {
		t.Fatalf("setup: draw %.0f W meter %.0f W", s.evW, s.gridW())
	}
	stepAt := s.now
	s.houseW += 4500 // 8,500 W, over the hour's limit with the car off.
	s.run(6 * time.Second)
	t.Logf("battery %.0f W after 2 s; meter %.0f W, limit %.0f W", since(s.targetW, stepAt.Add(2*time.Second))[0].w, s.gridW(), s.hourLimitW())
	for _, b := range since(s.targetW, stepAt.Add(2*time.Second)) {
		if b.w >= 0 {
			t.Fatalf("%v: no battery relief for an overage no cut removes (target %.0f W)", b.at.Sub(stepAt), b.w)
		}
	}
	if s.gridW() > s.hourLimitW()+50 {
		t.Fatalf("meter %.0f W over the hour's limit of %.0f W", s.gridW(), s.hourLimitW())
	}
}

// The car takes 2 kW of a 10,350 W offer, so a cut only lowers the offer.
// The overage may wait evSettle for the first cut, not again for the stop
// that follows it.
func TestCeilingOverageWaitsOnceWhenTheCarDrawsLessThanItsOffer(t *testing.T) {
	s := newIdleBatteryEVSite(t, -3000, 7500)
	s.carMaxW = 2000
	s.run(5 * time.Minute)
	if s.offer() != 10350 || s.evW != 2000 {
		t.Fatalf("setup: offer %.0f W draw %.0f W", s.offer(), s.evW)
	}
	stepAt := s.now
	s.houseW += 12600 // 11,600 W, a cut's worth over the hour's limit.
	s.run(time.Minute)
	reliefBy := stepAt.Add(time.Second + evSettle)
	t.Logf("offer %.0f W; battery %.0f W from %v", s.offer(), since(s.targetW, reliefBy)[0].w, reliefBy.Sub(stepAt))
	for _, b := range since(s.targetW, reliefBy) {
		if b.w >= 0 {
			t.Fatalf("%v: no battery relief after the overage waited %v (target %.0f W)", b.at.Sub(stepAt), evSettle, b.w)
		}
	}
	for _, m := range since(s.meterW, reliefBy.Add(time.Second)) {
		if m.w > s.hourLimitW()+50 {
			t.Fatalf("%v: meter %.0f W, the battery did not hold the hour's limit of %.0f W", m.at.Sub(stepAt), m.w, s.hourLimitW())
		}
	}
}

// A house step past the heat budget's instant limit (31 A on the 25 A fuse)
// cuts the car at once. The battery leaves the watts that cut removes, so
// the car and the battery never take one overage twice and nothing exports
// when the cut lands; the meter is back inside the fuse with the car's cut.
func TestFuseOverageLeftToTheCarsCut(t *testing.T) {
	s := newIdleBatteryEVSite(t, 3000, 0)
	s.run(5 * time.Minute)
	if s.offer() != 10350 || s.batteryW != 0 {
		t.Fatalf("setup: offer %.0f W battery %.0f W", s.offer(), s.batteryW)
	}
	stepAt := s.now
	s.houseW += 8000 // 21,350 W, 31 A on each phase.
	s.run(15 * time.Second)
	cutShows := stepAt.Add(time.Second + s.chargerDelay)
	for _, b := range since(s.targetW, stepAt) {
		if b.w < 0 {
			t.Fatalf("%v: the battery discharged %.0f W for watts the car's cut removes", b.at.Sub(stepAt), -b.w)
		}
	}
	for _, m := range since(s.meterW, cutShows.Add(time.Second)) {
		if m.w > 16560 || m.w < 0 {
			t.Fatalf("%v: meter %.0f W, want inside the fuse's 24 A and no export", m.at.Sub(stepAt), m.w)
		}
	}
	if s.offer() >= 10350 {
		t.Fatalf("the car was not cut: offer %.0f W", s.offer())
	}
}

// The battery's relief nets the EV cuts we sent during the run over a limit,
// for the hour's import limit and for the fuse alike, so the car and the
// battery never both take one overage (#1674). A run older than evSettle,
// or no run, nets nothing.
func TestFuseReliefNetsEVCutsSent(t *testing.T) {
	now := time.Date(2026, 10, 17, 16, 30, 0, 0, time.Local)
	hotPhase := map[string]float64{"l1_a": 27, "l2_a": 18, "l3_a": 18}
	cases := []struct {
		name              string
		ceilingW          float64
		gridW, commandedW float64
		runFor            time.Duration // the run over the limit; 0: none
		phaseA            map[string]float64
		wantW             float64
	}{
		{"ceiling overage the stop removes", 7500, 13660, 0, 5 * time.Second, nil, 0},
		{"ceiling overage beyond the cut", 7500, 13660, 4140, 5 * time.Second, nil, 13660 - 7500 - (9660 - 4140)},
		{"no cut", 7500, 13660, 9660, 5 * time.Second, nil, 13660 - 7500},
		{"overage older than evSettle", 7500, 13660, 0, evSettle, nil, 13660 - 7500},
		{"cut sent before this overage", 7500, 13660, 0, 0, nil, 13660 - 7500},
		{"phase over the fuse, no cut", 0, 13660, 9660, 5 * time.Second, hotPhase, (27 - 24) * 230 * 3},
		{"phase over the fuse, the stop covers it", 0, 13660, 0, 5 * time.Second, hotPhase, 0},
		{"phase over the fuse beyond the cut", 0, 13660, 8280, 5 * time.Second, hotPhase, (27 - 24 - 2) * 230 * 3},
	}
	for _, tc := range cases {
		st := NewState(0, 0, "meter")
		st.clock = func() time.Time { return now }
		st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
		st.PeakImportCeilingW = tc.ceilingW
		log := commandLog{{at: now.Add(-time.Hour), w: 9660, three: true}}
		if tc.runFor > 0 {
			log = append(log, sentCommand{at: now.Add(-tc.runFor), w: tc.commandedW, three: true})
			st.evLimit.overSince, st.evLimit.fuseOverSince = now.Add(-tc.runFor-time.Second), now.Add(-tc.runFor-time.Second)
		} else {
			log = append(log, sentCommand{at: now.Add(-time.Minute), w: tc.commandedW, three: true})
		}
		st.evLimit.offers = map[string]chargerOffers{"car": {driver: "easee", log: log, settle: evUnknownSettle}}
		st.SetEVCommanded([]loadpoint.State{{ID: "car", DriverName: "easee", PluggedIn: true, CommandedKnown: true, CommandedW: tc.commandedW}})
		store := telemetry.NewStore()
		data, _ := json.Marshal(tc.phaseA)
		store.Update("meter", telemetry.DerMeter, tc.gridW, nil, data)
		meter := store.Get("meter", telemetry.DerMeter)
		meter.UpdatedAt = now.Add(-time.Second)
		got := fuseReliefFromSnapshot([]DispatchTarget{{Driver: "pixii"}}, map[string]*telemetry.DerReading{"pixii": {}}, meter, st)
		if math.Abs(math.Max(0, got)-tc.wantW) > 1e-6 {
			t.Errorf("%s: relief %.0f W, want %.0f W", tc.name, got, tc.wantW)
		}
	}
}

// Two managed cars charge under the hour's ceiling, and one is unplugged
// during an overage. Its offer leaves the pool, but no cut of ours removed
// it, and the meter may already show it. The battery must not wait for it,
// though it still waits for a cut sent to the car that stays.
func TestUnpluggedCarIsNoCutTheBatteryWaitsFor(t *testing.T) {
	now := time.Date(2026, 10, 17, 16, 30, 0, 0, time.Local)
	st := NewState(0, 0, "meter")
	st.clock = func() time.Time { return now }
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
	st.PeakImportCeilingW = 7500
	store := telemetry.NewStore()
	configs := []loadpoint.Config{
		{ID: "a", DriverName: "easee-a", PhaseMode: "3p", MinChargeW: 4140},
		{ID: "b", DriverName: "easee-b", PhaseMode: "3p", MinChargeW: 4140},
	}
	// One control tick: the meter reads gridW, the guard steps on the cars'
	// offers, the controller leaves car a at aAfterW, then the battery asks
	// for relief.
	tick := func(gridW float64, bPlugged bool, aAfterW float64) float64 {
		now = now.Add(time.Second)
		a := gridW / 690
		data, _ := json.Marshal(map[string]any{"l1_a": a, "l2_a": a, "l3_a": a})
		store.Update("meter", telemetry.DerMeter, gridW, nil, data)
		store.Get("meter", telemetry.DerMeter).UpdatedAt = now
		store.Get("meter", telemetry.DerMeter).SmoothedW = gridW
		store.RecordDriverSuccess("meter")
		points := []loadpoint.State{
			{ID: "a", DriverName: "easee-a", PluggedIn: true, CommandedKnown: true, CommandedW: 5520},
			{ID: "b", DriverName: "easee-b", PluggedIn: bPlugged, CommandedKnown: true, CommandedW: 5520},
		}
		EVFuseBudget(store, st, points, configs, 17250)
		points[0].CommandedW = aAfterW
		st.SetEVCommanded(points)
		targets := []DispatchTarget{{Driver: "pixii"}}
		batteries := map[string]*telemetry.DerReading{"pixii": {}}
		return math.Max(0, fuseReliefFromSnapshot(targets, batteries, store.Get("meter", telemetry.DerMeter), st))
	}
	// 400 W over the ceiling, less than a step: the guard waits.
	if got := tick(7900, true, 5520); math.Abs(got-400) > 1e-6 {
		t.Fatalf("first overage: relief %.0f W, want 400 W", got)
	}
	// Car b is unplugged and the house adds 6 kW: 880 W over, no cut sent.
	if got := tick(7900-5520+6000, false, 5520); math.Abs(got-880) > 1e-6 {
		cutW, _ := st.evCutsSent(st.evLimit.overSince, now)
		t.Fatalf("after car b left: relief %.0f W, want 880 W; cuts sent %.0f W", got, cutW)
	}
	// 2,000 W over, and car a is stopped: that cut may still wait.
	if got := tick(9500, false, 0); got != 0 {
		t.Fatalf("after car a's stop: relief %.0f W, want 0 W", got)
	}
}

// Core stops car a during an overage of the hour's ceiling, and car b is
// unplugged 5 s later, before a's stop reaches the meter. b leaving is no
// cut of ours, but a's stop still is: the battery keeps waiting for it, and
// the site does not export when it lands.
func TestCutOnStayingCarStillWaitsWhenAnotherLeaves(t *testing.T) {
	start := time.Date(2026, 10, 17, 16, 30, 0, 0, time.Local)
	now := start
	st := NewState(0, 0, "meter")
	st.clock = func() time.Time { return now }
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
	st.PeakImportCeilingW = 7500
	store := telemetry.NewStore()
	configs := []loadpoint.Config{
		{ID: "a", DriverName: "easee-a", PhaseMode: "3p", MinChargeW: 4140},
		{ID: "b", DriverName: "easee-b", PhaseMode: "3p", MinChargeW: 4140},
	}
	const houseW = 2000
	batteryW, lowestMeter := 0.0, math.Inf(1)
	for tick := 0; tick < 15; tick++ {
		now = start.Add(time.Duration(tick) * time.Second)
		bPlugged := tick < 5
		aDrawW, bDrawW := 10350.0, 4140.0
		if tick >= 10 {
			aDrawW = 0 // a's stop, sent on the first tick, lands 10 s later.
		}
		if !bPlugged {
			bDrawW = 0
		}
		gridW := houseW + aDrawW + bDrawW + batteryW
		lowestMeter = math.Min(lowestMeter, gridW)
		a := gridW / 690
		data, _ := json.Marshal(map[string]any{"l1_a": a, "l2_a": a, "l3_a": a})
		store.Update("meter", telemetry.DerMeter, gridW, nil, data)
		store.Get("meter", telemetry.DerMeter).UpdatedAt = now
		store.Get("meter", telemetry.DerMeter).SmoothedW = gridW
		store.RecordDriverSuccess("meter")
		aOfferW := 10350.0 // a's offer before this tick
		if tick > 0 {
			aOfferW = 0
		}
		points := []loadpoint.State{
			{ID: "a", DriverName: "easee-a", PluggedIn: true, CommandedKnown: true, CommandedW: aOfferW},
			{ID: "b", DriverName: "easee-b", PluggedIn: bPlugged, CommandedKnown: true, CommandedW: 4140},
		}
		EVFuseBudget(store, st, points, configs, 17250)
		points[0].CommandedW = 0 // Core stops car a on the first tick.
		st.SetEVCommanded(points)
		targets := []DispatchTarget{{Driver: "pixii"}}
		batteries := map[string]*telemetry.DerReading{"pixii": {SmoothedW: batteryW}}
		relief := fuseReliefFromSnapshot(targets, batteries, store.Get("meter", telemetry.DerMeter), st)
		batteryW = -math.Max(0, relief) // The battery follows within a tick.
		if batteryW < 0 && tick < 10 {
			cutW, _ := st.evCutsSent(st.evLimit.overSince, now)
			t.Fatalf("%ds: battery %.0f W for watts a's stop removes; cuts sent %.0f W", tick, batteryW, cutW)
		}
	}
	if lowestMeter < 0 {
		t.Fatalf("the site exported %.0f W when a's stop landed", -lowestMeter)
	}
}
