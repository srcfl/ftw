package control

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/state"
)

// importPlanCase is the 25 October 2026 field case run closed-loop: an
// hourly import ceiling, 0.8 kW of house, a 9.6 kWh battery and a car that
// wants 19 kWh more by the deadline on a 3 × 20 A fuse. Core's planner
// replans every quarter from Core's own import hour and Core runs the plan.
type importPlanCase struct {
	start, deadline time.Time
	ceilingW, soc   float64
	restartAt       time.Duration // > 0: Core restarts then, with no record of the hour
}

type importPlanHour struct {
	start              time.Time
	plannedWh, metered float64
}

// runImportPlan returns each clock hour's import as the plan made at its
// start meant it and as the meter measured it, and the car's energy by the
// deadline as the first plan promised it and as delivered. It runs inside a
// synctest bubble.
func runImportPlan(t *testing.T, c importPlanCase) (hours []importPlanHour, promisedWh, deliveredWh float64) {
	t.Helper()
	zone := c.start.Location()
	time.Sleep(c.start.Sub(time.Now())) // The bubble's clock becomes the plant's.
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prices := make([]state.PricePoint, 32)
	for i := range prices {
		prices[i] = state.PricePoint{Zone: "SE3", SlotTsMs: c.start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: 40 + float64(i%4), TotalOreKwh: 90 + float64(i%4), Source: "test", FetchedAtMs: c.start.UnixMilli()}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := mpc.New(st, nil, "SE3", mpc.Params{Mode: mpc.ModeArbitrage, SoCLevels: 21, ActionLevels: 21,
		CapacityWh: 9600, InitialSoC: c.soc, SoCMin: .1, SoCMax: .95, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.Horizon, svc.BaseLoad = 8*time.Hour, 800
	svc.UpdateBatteryFleet([]mpc.BatteryFleetMember{{Driver: "battery", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000}}, 9600, 5000, 5000)
	svc.UpdateSiteEconomics(mpc.SiteEconomics{FuseMaxW: 13800, Timezone: zone.String()})

	p := sitesim.New(c.start, 20)
	p.EVSE = &sitesim.EVSE{Phases: 3, MinA: 6, MaxA: 16, Delay: 5 * time.Second, ReadbackLag: 10 * time.Second}
	p.Car = &sitesim.Car{Phases: 3, MaxW: 11040, Plugged: true}
	p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 9600, SoC: c.soc, Latency: 2 * time.Second}
	p.HouseW = [3]float64{800. / 3, 800. / 3, 800. / 3}
	if c.restartAt > 0 {
		p.Events = []sitesim.Event{{At: c.restartAt, Kind: sitesim.CoreRestart, For: 20 * time.Second}}
	}

	const carWh, needWh = 75000., 19000.
	s := &simSite{p: p, lp: simLoadpoint()}
	s.plan = func(now time.Time) (loadpoint.Directive, bool) {
		d, ok := svc.SlotDirectiveAt(now)
		if !ok {
			return loadpoint.Directive{}, false
		}
		return d.LoadpointDirective(), true
	}
	s.setup = func(s *simSite) {
		s.state.PeakImportCeilingW, s.state.importHourZone = c.ceilingW, zone
		s.state.Mode, s.state.UseEnergyDispatch = ModePlannerArbitrage, true
		s.state.SlotDirective = func(now time.Time) (SlotDirective, bool) {
			d, ok := svc.SlotDirectiveAt(now)
			if !ok {
				return SlotDirective{}, false
			}
			return SlotDirectiveFromMPC(d), true
		}
		s.mgr.SetSchedule(s.lp.ID, loadpoint.Schedule{SoC: .8})
	}
	s.boot()
	// The planner reads the hour the live limit counts, as main.go wires it.
	svc.ImportCeiling = func() mpc.ImportCeiling { return PlanningImportCeiling(s.state) }
	svc.Loadpoint = func(int) *mpc.LoadpointSpec {
		first := s.p.Now.Truncate(15 * time.Minute)
		return &mpc.LoadpointSpec{ID: s.lp.ID, CapacityWh: carWh, InitialSoC: (.8*carWh - needWh + .9*s.p.EVSE.SessionWh) / carWh, SoCMax: 1,
			Levels: 21, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: int(c.deadline.Sub(first)/(15*time.Minute)) - 1,
			MinChargeW: s.lp.MinChargeW, MaxChargeW: s.lp.MaxChargeW, AllowedStepsW: s.lp.AllowedStepsW, ChargeEfficiency: .9}
	}
	plannedWh := func(plan *mpc.Plan, from time.Time) float64 {
		var wh float64
		for _, a := range plan.Actions {
			begin, end := max(a.ExecutionStart(), from.UnixMilli()), min(a.SlotStartMs+int64(a.SlotLenMin)*60_000, from.Add(time.Hour).UnixMilli())
			if end > begin {
				wh += math.Max(0, a.GridW) * float64(end-begin) / 3.6e6
			}
		}
		return wh
	}

	nextReplan := c.start.Add(10 * time.Second) // Telemetry first.
	for s.p.Now.Before(c.deadline) {
		time.Sleep(sitesim.Step)
		s.step()
		now := s.p.Now
		if s.downUntil.IsZero() && !now.Before(nextReplan) {
			svc.Tele = s.store
			plan := svc.ReplanWithReason(context.Background(), "slot_elapsed")
			if plan == nil {
				t.Fatalf("%s: no plan: %+v", now.Format("15:04:05"), svc.PlanSnapshot())
			}
			if promisedWh == 0 {
				promisedWh = needWh - plan.LoadpointShortfallWh[s.lp.ID]
			}
			if h := importHourStart(now, zone); len(hours) == 0 || !h.Equal(hours[len(hours)-1].start) {
				hours = append(hours, importPlanHour{start: h, plannedWh: plannedWh(plan, h)})
			}
			nextReplan = now.Truncate(15 * time.Minute).Add(15*time.Minute + time.Second)
		}
		if len(hours) > 0 {
			hours[len(hours)-1].metered += math.Max(0, (p.A[0]+p.A[1]+p.A[2])*p.Voltage) * sitesim.Step.Hours()
		}
		// The site check's own idea of when a car should charge does not
		// know the plan; every other invariant holds.
		if s.failure != "" && !strings.Contains(s.failure, "idle") {
			t.Fatal(s.failure)
		}
	}
	return hours, promisedWh, .9 * s.p.EVSE.SessionWh
}

// Plan and execution agree: no clock hour imports more than the ceiling, and
// the car gets about what the first plan promised. With the battery covering
// the house the car runs from the hour's start and the promise holds within
// 3 %. Without it the car needs the hour's slack to start and Core restarts
// it each time a quarter's energy is spent: 8 %.
func TestSiteSimPlanUnderImportCeiling(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	nine := time.Date(2026, 10, 25, 9, 0, 0, 0, zone)
	night := time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC).In(zone) // 01:30 CEST, before 02:00 comes twice
	for _, c := range []struct {
		name    string
		c       importPlanCase
		promise float64
	}{
		{"battery covers the house", importPlanCase{start: nine, deadline: nine.Add(4 * time.Hour), ceilingW: 4500, soc: .5}, .03},
		{"battery empty", importPlanCase{start: nine, deadline: nine.Add(4 * time.Hour), ceilingW: 4500, soc: .1}, .08},
		{"the 25-hour day", importPlanCase{start: night, deadline: night.Add(4 * time.Hour), ceilingW: 4500, soc: .5}, .03},
		{"restart in the hour", importPlanCase{start: nine, deadline: nine.Add(2 * time.Hour), ceilingW: 4500, soc: .5, restartAt: 40 * time.Minute}, .05},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hours, promised, delivered := runImportPlan(t, c.c)
				var line string
				for _, h := range hours {
					line += fmt.Sprintf(" %s plan %.0f metered %.0f;", h.start.Format("15:04 MST"), h.plannedWh, h.metered)
					if h.metered > c.c.ceilingW {
						t.Errorf("%s: imported %.0f Wh, over the %.0f W ceiling", h.start.Format("15:04 MST"), h.metered, c.c.ceilingW)
					}
				}
				t.Logf("car promised %.0f Wh, delivered %.0f Wh;%s", promised, delivered, line)
				if math.Abs(delivered-promised) > c.promise*promised {
					t.Errorf("delivered %.0f Wh against %.0f Wh promised, more than %.0f %% apart", delivered, promised, c.promise*100)
				}
			})
		})
	}
}
