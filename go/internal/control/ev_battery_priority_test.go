package control

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// #1868: 16 A, 6 A on all three car phases, and 15.1 A on L1, then 0.1 A
// over the fuse less 1 A. The phase line is now the fuse less
// safety_margin_a, 15.5 A here, so the same readings leave 0.4 A of room
// (TestIssue1868ReadingsSitUnderTheLine). The car may wait for the battery
// only while every phase stays under that line, so the wait now serves the
// hour's import limit: the fixture is 69 W over a 9,015 W limit. The 4 kW
// battery is the continuous load that can yield first.
const issue1868CeilingW = 9015

func issue1868Site(t *testing.T) (*telemetry.Store, *State, []loadpoint.State, []loadpoint.Config) {
	t.Helper()
	tel, st := evSplitFixture(t, 9084, []float64{15.1, 12.5, 11.9}, 4140, "3p")
	st.SiteFuseAmps, st.SiteFuseSafetyA = 16, .5
	st.PeakImportCeilingW = issue1868CeilingW
	st.Mode = ModeCharge
	st.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 6000, MaxDischargeW: 6000}}
	soc := .4
	tel.Update("battery", telemetry.DerBattery, 4000, &soc, nil)
	tel.RecordDriverSuccess("battery")
	sent(st, "ev", 4140)
	// The car's 4140 W offer has run for longer than any charger's response time.
	a, n := st.offerPhaseA(4140, 3)
	st.evLimit.offers = map[string]chargerOffers{"rav4": {log: commandLog{{at: st.now().Add(-evUnknownSettle - time.Second), w: 4140, a: a, n: n}}}}
	sent(st, "battery", 4000)
	lastingFuseHeat(tel, st) // The overage has lasted, as in the field.
	points := []loadpoint.State{{ID: "rav4", DriverName: "car", PluggedIn: true, CommandedKnown: true, CommandedW: 4140, CommandedPhases: 3}}
	configs := []loadpoint.Config{{ID: "rav4", DriverName: "car", PhaseMode: "3p", MinChargeW: 4140, MaxChargeW: 4140, AllowedStepsW: []float64{0, 4140}}}
	return tel, st, points, configs
}

func TestIssue1868BatteryChargeYieldsBeforeTheCar(t *testing.T) {
	for _, mode := range AllModes() {
		t.Run(string(mode), func(t *testing.T) {
			tel, st, points, configs := issue1868Site(t)
			cap, known := EVFuseBudget(tel, st, points, configs, 11040)
			if !known || cap != 4140 {
				t.Fatalf("71 W relief shed the 4140 W car: cap=%v known=%v", cap, known)
			}
			st.SetEVCommanded(points)
			st.Mode = mode
			st.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "car", PowerW: 4140, PhaseMode: "3p"}})
			targets := ComputeDispatch(tel, st, map[string]float64{"battery": 12200}, 11040)
			if len(targets) != 1 {
				t.Fatalf("missing battery command: %+v", targets)
			}
			for _, target := range targets {
				if target.TargetW > 3931.001 {
					t.Fatalf("%s left the car's relief in the battery: %+v", mode, target)
				}
				// Charge mode asks for a raise. No phase reaches the 15.5 A
				// line, so only the hour's limit binds: the guard cuts the
				// 69 W overage to 3931 W, and the car's headroom then takes
				// the same 69 W again (3862 W).
				if mode == ModeCharge && target.TargetW < 3861 {
					t.Fatalf("small overage needlessly stopped battery charging: %+v", target)
				}
			}
			for _, target := range SelfTuneDispatch(tel, st, map[string]float64{"battery": 12200}, 11040, "battery", 6000) {
				if target.TargetW > 3931.001 {
					t.Fatalf("self-tune took the car's relief: %+v", target)
				}
			}
		})
	}
}

// With no import limit, #1868's readings sit 0.4 A under the 15.5 A line:
// nothing cuts the car and nothing waits for the battery.
func TestIssue1868ReadingsSitUnderTheLine(t *testing.T) {
	tel, st, points, configs := issue1868Site(t)
	st.PeakImportCeilingW = 0
	cap, known := EVFuseBudget(tel, st, points, configs, 11040)
	if !known || cap < 4140 || st.evLimit.batteryYield.active {
		t.Fatalf("cap=%v known=%v yield=%+v, want the 4140 W offer kept with no wait", cap, known, st.evLimit.batteryYield)
	}
}

func TestEVPriorityCannotSpendUnmeasuredBatteryRelief(t *testing.T) {
	tel, st, points, configs := issue1868Site(t)
	st.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "car", PowerW: 10350, PhaseMode: "3p"}})
	cap, _ := EVFuseBudget(tel, st, points, configs, 11040)
	if cap > points[0].CommandedW {
		t.Fatalf("battery cut became an unmeasured car rise: %v W", cap)
	}
}

func TestEVPriorityFallsBackWhenTheBatteryCannotYield(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*telemetry.Store, *State)
	}{
		{"no command", func(_ *telemetry.Store, st *State) { st.batteryCommands = nil }},
		{"not charging", func(_ *telemetry.Store, st *State) { sent(st, "battery", 0) }},
		{"insufficient charge", func(_ *telemetry.Store, st *State) { sent(st, "battery", 10) }},
		{"offline", func(tel *telemetry.Store, _ *State) { tel.Remove("battery") }},
		{"stale meter", func(tel *telemetry.Store, _ *State) {
			tel.Get("meter", telemetry.DerMeter).UpdatedAt = time.Now().Add(-time.Minute)
		}},
		{"over the working limit", func(tel *telemetry.Store, st *State) {
			tel.Update("meter", telemetry.DerMeter, 9000, nil, []byte(`{"l1_a":15.6,"l2_a":12.5,"l3_a":11.9}`))
			lastingFuseHeat(tel, st) // the overage has lasted (I²t)
		}},
		{"unseen battery rise", func(_ *telemetry.Store, st *State) { sent(st, "battery", 0, 4000) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel, st, points, configs := issue1868Site(t)
			tc.set(tel, st)
			cap, _ := EVFuseBudget(tel, st, points, configs, 11040)
			if cap >= 4140 || st.evLimit.batteryYield.active {
				t.Fatalf("unsafe battery priority: cap=%v yield=%+v", cap, st.evLimit.batteryYield)
			}
		})
	}
}

func TestEVPriorityWaitForBatteryHasAFixedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel, st, points, configs := issue1868Site(t)
		var deadline time.Time
		for tick := 0; tick <= 6; tick++ {
			// The battery accepts lower commands, but the site meter never
			// shows the reduction. Repeated commands cannot renew the wait.
			cap, _ := EVFuseBudget(tel, st, points, configs, 11040)
			if tick == 0 {
				deadline = st.evLimit.batteryYield.until
			}
			st.SetEVCommanded(points)
			if tick < 4 && cap != 4140 {
				t.Fatalf("tick %d: car cut before the response wait: %v", tick, cap)
			}
			if tick >= 4 && cap >= 4140 {
				t.Fatalf("tick %d: ignored battery cut holds the car forever: %v", tick, cap)
			}
			if st.evLimit.batteryYield.until != deadline {
				t.Fatalf("tick %d renewed the deadline", tick)
			}
			_ = ComputeDispatch(tel, st, map[string]float64{"battery": 12200}, 11040)
			time.Sleep(5 * time.Second)
			tel.Update("meter", telemetry.DerMeter, 9084, nil, []byte(`{"l1_a":15.1,"l2_a":12.5,"l3_a":11.9}`))
			tel.RecordDriverSuccess("meter")
		}
		t.Logf("car protected while battery responded; fallback deadline %s", fmt.Sprint(evSettle))
	})
}
