package mpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// carRoomSlot is one hour of the field night (fuseNightParams): a 3,800 W
// house under a 9,250 W limit, the battery at batteryW and the car at carW,
// pulsing at peakW when peakW is set.
func carRoomSlot(batteryW, carW, peakW float64) ([]Slot, Params, Plan) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 15, SpotOre: 15, LoadW: 3800,
		Limits: PowerLimits{MaxImportW: fuseNightImportW}}}
	p := fuseNightParams(ModeArbitrage, true)
	p.InitialSoC = .5
	p.Loadpoint.TargetSlotIdx = 0
	a := Action{SlotStartMs: 1, SlotLenMin: 60, LoadW: 3800, PriceOre: 15, SpotOre: 15,
		BatteryW: batteryW, GridW: 3800 + carW + batteryW,
		SoC:        .5 + loadpoint.BatteryEnergyDeltaWh(batteryW, 1, .95, .95)/p.CapacityWh,
		LoadpointW: carW, LoadpointSoC: .3 + carW*.9/84000}
	if peakW > 0 {
		a.LoadpointPowerW = map[string]float64{"car": carW}
		a.LoadpointMaxPowerW = map[string]float64{"car": peakW}
		a.LoadpointSoCByID = map[string]float64{"car": a.LoadpointSoC}
	}
	a.CostOre = reserveSlotCost(slots[0], p, a)
	return slots, p, Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC,
		Actions: []Action{a}, TotalCostOre: a.CostOre}
}

// A plan whose only broken rule is the car's import room is an overrun,
// which Core may publish when it cannot plan the site itself. A battery
// that discharges past the house still feeds the car, which no site allows.
func TestValidatePlanNamesTheCarRoomOverrun(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		batteryW, carW, peakW float64
		overrun               bool
	}{
		// The house uses all 3,000 W; with the battery idle the car could
		// have had only 5,450 W.
		{"steady car", -3000, 8280, 0, true},
		// The mean fits beside the house; the pulse does not.
		{"pulsing car", -3000, 4140, 8280, true},
		{"battery past the house", -5000, 8280, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, p, plan := carRoomSlot(tc.batteryW, tc.carW, tc.peakW)
			err := ValidatePlan(slots, p, &plan)
			var overrun *planOverrun
			if err == nil || errors.As(err, &overrun) != tc.overrun {
				t.Fatalf("err %v, overrun %v; want overrun %v", err, overrun != nil, tc.overrun)
			}
			if tc.overrun && (overrun.carRoom == nil || overrun.hours != nil || len(overrun.plan.Actions) != 1) {
				t.Fatalf("overrun %+v", overrun)
			}
		})
	}
}

// carRoomOptimizer plans as the fleet worker does on the field night: in
// the first slot where the house draws, the battery covers the house and
// the car charges on the room that frees. Every other rule holds. A
// downside worker plans the caution forecast first, as Energyplan does.
type carRoomOptimizer struct{ downside bool }

func (o carRoomOptimizer) UsesDownsidePV() bool { return o.downside }

func (carRoomOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	lp := p.activeLoadpoints()[0]
	plan := Plan{Mode: p.Mode, HorizonSlots: len(slots), CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC,
		Solver: &SolverInfo{Engine: "native", Backend: "car-room-test", Status: "feasible"}}
	energy := make(map[string]float64, len(p.Storages))
	for _, s := range p.Storages {
		energy[s.ID] = s.InitialEnergyWh
	}
	soc, carSoC, placed := p.InitialSoC, lp.InitialSoC, false
	for _, slot := range slots {
		a := Action{SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin, ExecutionStartMs: slot.ExecutionStartMs,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre, LoadW: slot.LoadW, PVW: slot.PVW}
		house, limit := slot.LoadW+slot.PVW, slot.Limits.MaxImportW
		if !placed && house > 100 && limit > 0 {
			for _, step := range lp.normalizedSteps() {
				if house+step > limit+50 && step <= limit-50 {
					a.BatteryW, a.LoadpointW, placed = -house, step, true
					break
				}
			}
		}
		dtH := slot.DurationHours()
		soc += loadpoint.BatteryEnergyDeltaWh(a.BatteryW, dtH, p.ChargeEfficiency, p.DischargeEfficiency) / p.CapacityWh
		if len(p.Storages) > 0 {
			a.StoragePowerW = map[string]float64{}
			a.StorageEnergyWh = map[string]float64{}
			total := 0.0
			for i, s := range p.Storages {
				w := 0.0
				if i == 0 {
					w = a.BatteryW
				}
				energy[s.ID] += loadpoint.BatteryEnergyDeltaWh(w, dtH, s.ChargeEfficiency, s.DischargeEfficiency)
				a.StoragePowerW[s.ID], a.StorageEnergyWh[s.ID] = w, energy[s.ID]
				total += energy[s.ID]
			}
			soc = total / p.CapacityWh
		}
		carSoC += a.LoadpointW * dtH * lp.ChargeEfficiency / lp.CapacityWh
		a.SoC, a.LoadpointSoC = soc, carSoC
		a.GridW = loadpoint.GridW(slot.LoadW, slot.PVW, a.BatteryW, a.LoadpointW)
		a.CostOre = reserveSlotCost(slot, p, a)
		plan.TotalCostOre += a.CostOre
		plan.Actions = append(plan.Actions, a)
	}
	if !placed {
		return Plan{}, errors.New("test site has no slot where the battery makes the car's room")
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		return Plan{}, errors.Join(errors.New("optimizer plan rejected"), err)
	}
	return plan, nil
}

func (carRoomOptimizer) Close() error { return nil }

// carRoomFieldService is the import-ceiling field site on a 9,250 W fuse,
// with a car the battery may not cover and storages home batteries.
func carRoomFieldService(t *testing.T, optimizer PlanOptimizer, storages int) *Service {
	t.Helper()
	svc, _ := importCeilingFieldService(t, optimizer, false)
	svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: fuseNightImportW, Timezone: "Europe/Stockholm"})
	car := svc.Loadpoint
	svc.Loadpoint = func(i int) *LoadpointSpec {
		lp := car(i)
		lp.NoBatteryToEV = true
		return lp
	}
	if storages == 2 {
		soc := .5
		svc.Tele.Update("pixii", telemetry.DerBattery, 0, &soc, nil)
		svc.Tele.DriverHealthMut("pixii").RecordSuccess()
		svc.UpdateBatteryFleet([]BatteryFleetMember{
			{Driver: "sungrow", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000},
			{Driver: "pixii", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000},
		}, 19200, 10000, 10000)
	}
	return svc
}

// A worker plan in which the battery makes the car's import room falls back
// to Core's own plan. With two batteries Core cannot plan the site itself:
// the worker's plan is published with a warning rather than no new plan,
// and the charger's live room still leaves the battery's discharge out.
func TestCarRoomWorkerPlanWhenCoreCannotPlan(t *testing.T) {
	for _, storages := range []int{1, 2} {
		svc := carRoomFieldService(t, carRoomOptimizer{}, storages)
		if svc.Replan(context.Background()) == nil {
			t.Fatalf("%d storages: no plan published: %+v", storages, svc.PlanSnapshot())
		}
		snap := svc.PlanSnapshot()
		warned := strings.Contains(strings.Join(snap.Warnings, " "), "lets the battery make room for the car")
		if storages == 1 {
			if !snap.Plan.Solver.Fallback || warned {
				t.Fatalf("1 storage: solver %+v, warnings %q; want Core's own plan", snap.Plan.Solver, snap.Warnings)
			}
			continue
		}
		if snap.Plan.Solver.Fallback || snap.Plan.Solver.Backend != "car-room-test" || !warned {
			t.Fatalf("2 storages: solver %+v, warnings %q; want the worker's plan with a warning", snap.Plan.Solver, snap.Warnings)
		}
	}
}

// A worker that plans the caution forecast first returns only its point
// plan's error once caution fails. Core publishes that point plan, so the
// plan reads as one without the extra forecast margin.
func TestCarRoomPointPlanIsWithoutCaution(t *testing.T) {
	svc := carRoomFieldService(t, carRoomOptimizer{downside: true}, 2)
	svc.PVForecastSafetyK = .3
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		return ForecastInputs{Load: func(time.Time) float64 { return 800 },
			Risk: func(_, risk []Slot, _ float64) {
				for i := range risk {
					risk[i].LoadW = 900
				}
			}}
	}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Backend != "car-room-test" {
		t.Fatalf("no worker plan published: %+v", svc.PlanSnapshot())
	}
	if !plan.CautionOff || !strings.Contains(strings.Join(svc.PlanSnapshot().Warnings, " "), "caution") {
		t.Fatalf("caution off = %v, warnings %q; want the point plan marked without caution", plan.CautionOff, svc.PlanSnapshot().Warnings)
	}
}

// The bundled fleet worker plans a two-battery site with the field night's
// house and fuse. Core cannot plan the site itself, so it publishes the
// worker's plan rather than no new plan.
func TestNativeFleetCarRoomWhenCoreCannotPlan(t *testing.T) {
	worker := nativeWorker(t, 1500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	svc := carRoomFieldService(t, worker, 2)
	svc.BaseLoad = 3800
	if svc.Replan(context.Background()) == nil {
		t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
	}
	t.Logf("solver %+v, warnings %q", svc.PlanSnapshot().Plan.Solver, svc.PlanSnapshot().Warnings)
}
