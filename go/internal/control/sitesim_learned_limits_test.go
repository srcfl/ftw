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

	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/state"
)

// learnedNightQuarter is one quarter of the night: the battery energy the
// plan in force wanted from it and what the battery gave.
type learnedNightQuarter struct {
	start              time.Time
	plannedWh, givenWh float64
}

// runLearnedNight runs the owner's home box at night closed-loop: a 9.6 kWh
// battery at 10 % behind an inverter that holds grid import at 4.32 kW while
// it charges, 1.2 kW of house, two cheap hours and two dear ones. Core's
// planner replans every quarter and Core runs the plan. learned gives the
// planner the import hold the battery model has learned.
func runLearnedNight(t *testing.T, learned bool) []learnedNightQuarter {
	t.Helper()
	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	time.Sleep(start.Sub(time.Now())) // The bubble's clock becomes the plant's.
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prices := make([]state.PricePoint, 32)
	for i := range prices {
		total := 30.
		if i >= 8 {
			total = 300
		}
		prices[i] = state.PricePoint{Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: total - 50, TotalOreKwh: total, Source: "test", FetchedAtMs: start.UnixMilli()}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := mpc.New(st, nil, "SE3", mpc.Params{Mode: mpc.ModeArbitrage, SoCLevels: 21, ActionLevels: 21,
		CapacityWh: 9600, InitialSoC: .1, SoCMin: .05, SoCMax: .95, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.Horizon, svc.BaseLoad = 4*time.Hour, 1200
	svc.UpdateBatteryFleet([]mpc.BatteryFleetMember{{Driver: "battery", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000}}, 9600, 5000, 5000)
	svc.UpdateSiteEconomics(mpc.SiteEconomics{FuseMaxW: 17250, Timezone: "UTC"})
	if learned {
		svc.LearnedBatteryLimits = func(driver string, _ *float64) []battery.Plateau {
			if driver != "battery" {
				return nil
			}
			return []battery.Plateau{{Direction: "charge", Kind: battery.LimitGridImport, Value: 4320, Unit: "W", Seen: 9}}
		}
	}

	p := sitesim.New(start, 25)
	p.EVSE = &sitesim.EVSE{Phases: 3, MinA: 6, MaxA: 16, Delay: 5 * time.Second, ReadbackLag: 10 * time.Second}
	p.Car = &sitesim.Car{Phases: 3, MaxW: 11040}
	p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 9600, SoC: .1,
		Latency: 2 * time.Second, GridImportCapW: 4320}
	p.HouseW = [3]float64{400, 400, 400}
	s := &simSite{p: p, lp: simLoadpoint()}
	s.setup = func(s *simSite) {
		s.state.Mode, s.state.UseEnergyDispatch = ModePlannerArbitrage, true
		s.state.SlotDirective = func(now time.Time) (SlotDirective, bool) {
			d, ok := svc.SlotDirectiveAt(now)
			if !ok {
				return SlotDirective{}, false
			}
			return SlotDirectiveFromMPC(d), true
		}
	}
	s.boot()

	var quarters []learnedNightQuarter
	end := start.Add(2 * time.Hour)
	nextReplan := start.Add(10 * time.Second) // Telemetry first.
	for s.p.Now.Before(end) {
		time.Sleep(sitesim.Step)
		s.step()
		now := s.p.Now
		if !now.Before(nextReplan) {
			svc.Tele = s.store
			plan := svc.ReplanWithReason(context.Background(), "slot_elapsed")
			if plan == nil {
				t.Fatalf("%s: no plan: %+v", now.Format("15:04:05"), svc.PlanSnapshot())
			}
			q := now.Truncate(15 * time.Minute)
			if len(quarters) == 0 || !quarters[len(quarters)-1].start.Equal(q) {
				var wh float64
				for _, a := range plan.Actions {
					begin, end := max(a.ExecutionStart(), now.UnixMilli()), a.SlotStartMs+int64(a.SlotLenMin)*60_000
					if a.SlotStartMs == q.UnixMilli() && end > begin {
						wh = math.Max(0, a.BatteryW) * float64(end-begin) / 3.6e6
					}
				}
				quarters = append(quarters, learnedNightQuarter{start: q, plannedWh: wh})
			}
			nextReplan = q.Add(15*time.Minute + time.Second)
		}
		if len(quarters) > 0 {
			quarters[len(quarters)-1].givenWh += math.Max(0, p.Battery.W) * sitesim.Step.Hours()
		}
		if s.failure != "" && !strings.Contains(s.failure, "idle") {
			t.Fatal(s.failure)
		}
	}
	return quarters
}

// The planner that knows the inverter's import hold plans the night charge
// the battery can take, so every cheap quarter gives what its plan wanted.
// The planner without it plans 5 kW and every cheap quarter falls short.
func TestSiteSimNightChargeWithinTheLearnedImportHold(t *testing.T) {
	for _, learned := range []bool{false, true} {
		t.Run(fmt.Sprintf("learned=%v", learned), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				quarters := runLearnedNight(t, learned)
				short, line := 0, ""
				for _, q := range quarters {
					line += fmt.Sprintf(" %s plan %.0f gave %.0f;", q.start.Format("15:04"), q.plannedWh, q.givenWh)
					if q.plannedWh-q.givenWh > 50 {
						short++
					}
				}
				t.Logf("%d of %d quarters short:%s", short, len(quarters), line)
				if learned && short > 0 {
					t.Errorf("%d quarters gave less than their plan, with the learned hold", short)
				}
				if !learned && short < 4 {
					t.Errorf("only %d quarters short without the learned hold: the case shows nothing", short)
				}
			})
		})
	}
}
