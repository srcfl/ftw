package mpc

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A Pixii Home 20 kWh stops charging at 90 % while the planner's ceiling is
// 95 %. The plan said "90 → 95 %, 1.0 kWh" and dispatch ordered charge the
// battery never took (srcfl/device-drivers#175). The driver now reports the
// device's ceiling as max_soc_fract.

func TestOnlineFleetParamsPlansEachBatteryToItsDeviceCeiling(t *testing.T) {
	tel := telemetry.NewStore()
	socPixii, socOther := .9, .5
	tel.Update("pixii", telemetry.DerBattery, 0, &socPixii, json.RawMessage(`{"max_soc_fract":0.9}`))
	tel.DriverHealthMut("pixii").RecordSuccess()
	tel.Update("other", telemetry.DerBattery, 0, &socOther, json.RawMessage(`{"max_soc_fract":0.98}`))
	tel.DriverHealthMut("other").RecordSuccess()

	s := &Service{Tele: tel}
	p, ok := s.onlineFleetParams(Params{InitialSoC: .5, SoCMin: .1, SoCMax: .95, ChargeEfficiency: .95, DischargeEfficiency: .95}, []BatteryFleetMember{
		{Driver: "pixii", CapacityWh: 20000, MaxChargeW: 5000, MaxDischargeW: 5000},
		{Driver: "other", CapacityWh: 10000, MaxChargeW: 5000, MaxDischargeW: 5000},
	}, 0)
	if !ok {
		t.Fatal("onlineFleetParams returned ok=false")
	}
	if got := p.Storages[0].MaxEnergyWh; math.Abs(got-18000) > 1e-6 {
		t.Fatalf("pixii MaxEnergyWh = %.1f, want 18000 at its own 90%%", got)
	}
	if got := p.Storages[1].MaxEnergyWh; math.Abs(got-9500) > 1e-6 {
		t.Fatalf("other MaxEnergyWh = %.1f, want the planner's 95%%: a higher device ceiling never lifts it", got)
	}
	// (18 + 9.5) / 30 kWh.
	if want := 27500.0 / 30000; math.Abs(p.SoCMax-want) > 1e-9 {
		t.Fatalf("fleet SoCMax = %.6f, want %.6f", p.SoCMax, want)
	}
	if err := validateStorageSpecs(p, map[string]string{}); err != nil {
		t.Fatalf("storages no longer add up to the fleet: %v", err)
	}
}

func TestOnlineFleetParamsIgnoresDeviceCeilingAtTheFloor(t *testing.T) {
	tel := telemetry.NewStore()
	soc := .1
	tel.Update("pixii", telemetry.DerBattery, 0, &soc, json.RawMessage(`{"max_soc_fract":0.1}`))
	tel.DriverHealthMut("pixii").RecordSuccess()
	s := &Service{Tele: tel}
	p, ok := s.onlineFleetParams(Params{SoCMin: .1, SoCMax: .95}, []BatteryFleetMember{
		{Driver: "pixii", CapacityWh: 20000, MaxChargeW: 5000, MaxDischargeW: 5000},
	}, 0)
	if !ok || p.SoCMax != .95 || p.Storages[0].MaxEnergyWh != 19000 {
		t.Fatalf("a device ceiling at the SoC floor changed the plan's window: ok=%v SoCMax=%v storage=%+v",
			ok, p.SoCMax, p.Storages[0])
	}
}

// deviceCeilingPlan replans the field case: a 20 kWh battery at 90 % with
// cheap hours first and dear hours after, the prices that made the plan book
// a top-up to 95 %.
func deviceCeilingPlan(t *testing.T, data string) Plan {
	t.Helper()
	start := time.Date(2026, 11, 24, 1, 0, 0, 0, stockholm(t))
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prices := make([]state.PricePoint, 32)
	for i := range prices {
		spot := 10.0
		if i >= 8 {
			spot = 300
		}
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: spot, TotalOreKwh: spot + 60, Source: "test", FetchedAtMs: start.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModeArbitrage, SoCLevels: 101, ActionLevels: 21,
		CapacityWh: 20000, InitialSoC: .9, SoCMin: .1, SoCMax: .95,
		MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: .95, DischargeEfficiency: .95,
	})
	svc.Horizon = 8 * time.Hour
	svc.BaseLoad = 500
	svc.now = func() time.Time { return start.Add(30 * time.Second) }
	tel := telemetry.NewStore()
	soc := .9
	tel.Update("pixii", telemetry.DerBattery, 0, &soc, json.RawMessage(data))
	tel.DriverHealthMut("pixii").RecordSuccess()
	svc.Tele = tel
	svc.UpdateBatteryFleet([]BatteryFleetMember{{Driver: "pixii", CapacityWh: 20000, MaxChargeW: 5000, MaxDischargeW: 5000}}, 20000, 5000, 5000)
	svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: 13800, Timezone: "Europe/Stockholm"})
	if svc.Replan(context.Background()) == nil {
		t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
	}
	return *svc.PlanSnapshot().Plan
}

func TestPlanNeverTargetsAboveTheDeviceCeiling(t *testing.T) {
	highest := func(plan Plan) float64 {
		top := 0.0
		for _, a := range plan.Actions {
			top = math.Max(top, a.SoC)
		}
		return top
	}
	// Without the device's ceiling the plan tops the battery up to 95 %,
	// which is the field finding.
	if top := highest(deviceCeilingPlan(t, `{}`)); top < .93 {
		t.Fatalf("control case peaks at %.3f; the prices no longer make the plan charge", top)
	}
	plan := deviceCeilingPlan(t, `{"max_soc_fract":0.9}`)
	if top := highest(plan); top > .9+1e-9 {
		t.Fatalf("plan targets %.3f above the device's 90%% ceiling", top)
	}
	for _, a := range plan.Actions {
		if a.SoC >= .9-1e-9 && a.BatteryW > IdleGateThresholdW {
			t.Fatalf("plan charges %.0f W at the device ceiling: %+v", a.BatteryW, a)
		}
	}
}
