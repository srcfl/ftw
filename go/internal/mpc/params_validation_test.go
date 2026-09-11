package mpc

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func validPlanningParams() Params {
	p := baseParams(ModePassiveArbitrage)
	p.PVChargeBonusOreKwh = 10
	p.ExportBonusOreKwh = 5
	p.ExportFeeOreKwh = 2
	p.MinArbitrageSpreadOreKwh = 20
	p.PVUncertaintyW = 300
	p.PVForecastSafetyK = 1
	return p
}

func validPlanningStorageParams() Params {
	p := validPlanningParams()
	p.Storages = []StorageAssetSpec{{
		ID: "battery", CapacityWh: p.CapacityWh,
		InitialEnergyWh: p.CapacityWh * p.InitialSoC,
		MinEnergyWh:     p.CapacityWh * p.SoCMin,
		MaxEnergyWh:     p.CapacityWh * p.SoCMax,
		MaxChargeW:      p.MaxChargeW, MaxDischargeW: p.MaxDischargeW,
		ChargeEfficiency: p.ChargeEfficiency, DischargeEfficiency: p.DischargeEfficiency,
	}}
	return p
}

func validPlanningLoadpoint() *LoadpointSpec {
	return &LoadpointSpec{
		ID: "garage", CapacityWh: 60000, Levels: 11,
		SoCMin: 0, SoCMax: 1.0, InitialSoC: 0.2, PluggedIn: true,
		TargetSoC: 0.8, TargetSlotIdx: 8,
		MaxChargeW: 11000, AllowedStepsW: []float64{0, 1400, 4100, 11000},
		ChargeEfficiency: 0.9,
	}
}

func TestValidateServiceGridLimits(t *testing.T) {
	valid := []struct {
		name       string
		fuseMaxW   float64
		maxExportW float64
	}{
		{name: "disabled", fuseMaxW: 0, maxExportW: 0},
		{name: "fuse only", fuseMaxW: 11000, maxExportW: 0},
		{name: "tighter export ceiling", fuseMaxW: 11000, maxExportW: 8000},
		{name: "export ceiling above fuse", fuseMaxW: 11000, maxExportW: 12000},
	}
	for _, tc := range valid {
		t.Run("accept "+tc.name, func(t *testing.T) {
			if err := validateServiceGridLimits(tc.fuseMaxW, tc.maxExportW); err != nil {
				t.Fatalf("valid grid limits rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name       string
		fuseMaxW   float64
		maxExportW float64
		want       string
	}{
		{name: "negative fuse", fuseMaxW: -1, want: "fuse_max_w"},
		{name: "nan fuse", fuseMaxW: math.NaN(), want: "fuse_max_w"},
		{name: "positive infinite fuse", fuseMaxW: math.Inf(1), want: "fuse_max_w"},
		{name: "negative infinite fuse", fuseMaxW: math.Inf(-1), want: "fuse_max_w"},
		{name: "negative export", fuseMaxW: 11000, maxExportW: -1, want: "max_export_w"},
		{name: "nan export", fuseMaxW: 11000, maxExportW: math.NaN(), want: "max_export_w"},
		{name: "positive infinite export", fuseMaxW: 11000, maxExportW: math.Inf(1), want: "max_export_w"},
		{name: "negative infinite export", fuseMaxW: 11000, maxExportW: math.Inf(-1), want: "max_export_w"},
	}
	for _, tc := range invalid {
		t.Run("reject "+tc.name, func(t *testing.T) {
			err := validateServiceGridLimits(tc.fuseMaxW, tc.maxExportW)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}
}

func TestValidatePlanningParamsAcceptsSupportedPhysicalStates(t *testing.T) {
	modes := []Mode{ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage, ModeArbitrage}
	for _, mode := range modes {
		p := validPlanningParams()
		p.Mode = mode
		if err := validatePlanningParams(p); err != nil {
			t.Fatalf("mode %q rejected: %v", mode, err)
		}
	}

	tests := []struct {
		name   string
		mutate func(*Params)
	}{
		{"physical minimum and maximum", func(p *Params) { p.SoCMin, p.SoCMax = 0, 1 }},
		{"below operating minimum recovery", func(p *Params) { p.InitialSoC = 0.05 }},
		{"above operating maximum recovery", func(p *Params) { p.InitialSoC = 0.97 }},
		{"disabled power", func(p *Params) { p.MaxChargeW, p.MaxDischargeW = 0, 0 }},
		{"ideal efficiency", func(p *Params) { p.ChargeEfficiency, p.DischargeEfficiency = 1, 1 }},
		{"finite negative prices", func(p *Params) {
			p.TerminalSoCPrice, p.ExportOrePerKWh, p.ExportBonusOreKwh, p.ExportFeeOreKwh = -10, -20, -30, -40
		}},
		{"storage below operating minimum recovery", func(p *Params) {
			*p = validPlanningStorageParams()
			p.InitialSoC = 0.05
			p.Storages[0].InitialEnergyWh = 500
		}},
		{"storage above operating maximum recovery", func(p *Params) {
			*p = validPlanningStorageParams()
			p.InitialSoC = 0.97
			p.Storages[0].InitialEnergyWh = 9700
		}},
		{"loadpoint documented default efficiency", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.SoCMin, lp.SoCMax = 0, 0
			lp.ChargeEfficiency = 0
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"loadpoint zero max shorthand", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.MaxChargeW = 0
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"loadpoint zero target sentinel", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.SoCMin, lp.TargetSoC = 0.1, 0
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"loadpoint zero target ignores negative deadline", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.TargetSoC = 0
			lp.TargetSlotIdx = -1
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"loadpoint first-slot deadline", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.TargetSlotIdx = 0
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"loadpoint deadline past horizon", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.TargetSlotIdx = 10000
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
		}},
		{"equivalent normalized loadpoint fallback", func(p *Params) {
			lp := validPlanningLoadpoint()
			lp.SoCMin, lp.SoCMax = 0, 0
			lp.ChargeEfficiency = 0
			lp.AllowedStepsW = []float64{4100, 0, 1400, 4100}
			fallback := *lp
			fallback.SoCMin, fallback.SoCMax = 0, 1
			fallback.ChargeEfficiency = 0.9
			fallback.AllowedStepsW = []float64{1400, 4100}
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = &fallback
		}},
		{"unplugged invalid loadpoint ignored", func(p *Params) {
			p.Loadpoints = []*LoadpointSpec{{PluggedIn: false, CapacityWh: math.NaN()}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlanningParams()
			tc.mutate(&p)
			if err := validatePlanningParams(p); err != nil {
				t.Fatalf("valid state rejected: %v", err)
			}
		})
	}
}

func TestValidatePlanningParamsRejectsInvalidAggregateValues(t *testing.T) {
	floorNaN := math.NaN()
	tests := []struct {
		name   string
		want   string
		mutate func(*Params)
	}{
		{"empty mode", "unsupported mode", func(p *Params) { p.Mode = "" }},
		{"unknown mode", "unsupported mode", func(p *Params) { p.Mode = "fast" }},
		{"soc levels", "soc_levels", func(p *Params) { p.SoCLevels = 2 }},
		{"action levels", "action_levels", func(p *Params) { p.ActionLevels = 2 }},
		{"zero capacity", "capacity_wh", func(p *Params) { p.CapacityWh = 0 }},
		{"nan capacity", "capacity_wh", func(p *Params) { p.CapacityWh = math.NaN() }},
		{"negative soc minimum", "soc bounds", func(p *Params) { p.SoCMin = -1 }},
		{"equal soc bounds", "soc bounds", func(p *Params) { p.SoCMax = p.SoCMin }},
		{"reversed soc bounds", "soc bounds", func(p *Params) { p.SoCMin = 0.96 }},
		{"soc maximum above physical", "soc bounds", func(p *Params) { p.SoCMax = 1.01 }},
		{"nan soc bound", "soc bounds", func(p *Params) { p.SoCMin = math.NaN() }},
		{"initial soc below physical", "initial_soc", func(p *Params) { p.InitialSoC = -0.1 }},
		{"initial soc above physical", "initial_soc", func(p *Params) { p.InitialSoC = 1.001 }},
		{"initial soc infinite", "initial_soc", func(p *Params) { p.InitialSoC = math.Inf(1) }},
		{"negative charge power", "max_charge_w", func(p *Params) { p.MaxChargeW = -1 }},
		{"infinite discharge power", "max_discharge_w", func(p *Params) { p.MaxDischargeW = math.Inf(1) }},
		{"zero charge efficiency", "charge_efficiency", func(p *Params) { p.ChargeEfficiency = 0 }},
		{"high charge efficiency", "charge_efficiency", func(p *Params) { p.ChargeEfficiency = 1.01 }},
		{"nan discharge efficiency", "discharge_efficiency", func(p *Params) { p.DischargeEfficiency = math.NaN() }},
		{"nan terminal price", "terminal_soc_price", func(p *Params) { p.TerminalSoCPrice = math.NaN() }},
		{"infinite export flat", "export_ore_per_kwh", func(p *Params) { p.ExportOrePerKWh = math.Inf(1) }},
		{"nan export bonus", "export_bonus_ore_kwh", func(p *Params) { p.ExportBonusOreKwh = math.NaN() }},
		{"infinite export fee", "export_fee_ore_kwh", func(p *Params) { p.ExportFeeOreKwh = math.Inf(-1) }},
		{"nan export floor", "export_floor_ore_kwh", func(p *Params) { p.ExportFloorOreKwh = &floorNaN }},
		{"negative pv bonus", "pv_charge_bonus", func(p *Params) { p.PVChargeBonusOreKwh = -1 }},
		{"negative spread", "min_arbitrage_spread", func(p *Params) { p.MinArbitrageSpreadOreKwh = -1 }},
		{"negative uncertainty", "pv_uncertainty", func(p *Params) { p.PVUncertaintyW = -1 }},
		{"negative relative uncertainty", "pv_relative_uncertainty", func(p *Params) { p.PVRelativeUncertainty = -1 }},
		{"nan relative uncertainty", "pv_relative_uncertainty", func(p *Params) { p.PVRelativeUncertainty = math.NaN() }},
		{"nan safety", "pv_forecast_safety", func(p *Params) { p.PVForecastSafetyK = math.NaN() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlanningParams()
			tc.mutate(&p)
			err := validatePlanningParams(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}
}

func TestValidatePlanningParamsRejectsInvalidStoragePhysics(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*Params)
	}{
		{"empty id", ".id", func(p *Params) { p.Storages[0].ID = "" }},
		{"duplicate id", "duplicates", func(p *Params) { p.Storages = append(p.Storages, p.Storages[0]) }},
		{"zero capacity", ".capacity_wh", func(p *Params) { p.Storages[0].CapacityWh = 0 }},
		{"nan capacity", ".capacity_wh", func(p *Params) { p.Storages[0].CapacityWh = math.NaN() }},
		{"negative minimum", "energy bounds", func(p *Params) { p.Storages[0].MinEnergyWh = -1 }},
		{"nan minimum", "energy bounds", func(p *Params) { p.Storages[0].MinEnergyWh = math.NaN() }},
		{"infinite maximum", "energy bounds", func(p *Params) { p.Storages[0].MaxEnergyWh = math.Inf(1) }},
		{"maximum above capacity", "energy bounds", func(p *Params) { p.Storages[0].MaxEnergyWh = 10001 }},
		{"initial below physical", "initial_energy_wh", func(p *Params) { p.Storages[0].InitialEnergyWh = -1 }},
		{"nan initial", "initial_energy_wh", func(p *Params) { p.Storages[0].InitialEnergyWh = math.NaN() }},
		{"initial above physical", "initial_energy_wh", func(p *Params) { p.Storages[0].InitialEnergyWh = 10001 }},
		{"negative charge power", ".max_charge_w", func(p *Params) { p.Storages[0].MaxChargeW = -1 }},
		{"nan discharge power", ".max_discharge_w", func(p *Params) { p.Storages[0].MaxDischargeW = math.NaN() }},
		{"zero efficiency", ".charge_efficiency", func(p *Params) { p.Storages[0].ChargeEfficiency = 0 }},
		{"nan efficiency", ".charge_efficiency", func(p *Params) { p.Storages[0].ChargeEfficiency = math.NaN() }},
		{"high efficiency", ".discharge_efficiency", func(p *Params) { p.Storages[0].DischargeEfficiency = 1.01 }},
		{"capacity aggregate mismatch", "aggregate capacity", func(p *Params) { p.Storages[0].CapacityWh += 10 }},
		{"initial aggregate mismatch", "aggregate initial", func(p *Params) { p.Storages[0].InitialEnergyWh += 10 }},
		{"minimum aggregate mismatch", "aggregate min", func(p *Params) { p.Storages[0].MinEnergyWh += 10 }},
		{"maximum aggregate mismatch", "aggregate max", func(p *Params) { p.Storages[0].MaxEnergyWh -= 10 }},
		{"charge aggregate mismatch", "aggregate max_charge", func(p *Params) { p.Storages[0].MaxChargeW -= 3 }},
		{"discharge aggregate mismatch", "aggregate max_discharge", func(p *Params) { p.Storages[0].MaxDischargeW -= 3 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlanningStorageParams()
			tc.mutate(&p)
			err := validatePlanningParams(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}
}

func TestValidatePlanningParamsRejectsInvalidLoadpointPhysics(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*Params)
	}{
		{"empty id", ".id", func(p *Params) { p.Loadpoints[0].ID = "" }},
		{"duplicate id", "duplicates", func(p *Params) { p.Loadpoints = append(p.Loadpoints, validPlanningLoadpoint()) }},
		{"zero capacity", ".capacity_wh", func(p *Params) { p.Loadpoints[0].CapacityWh = 0 }},
		{"nan capacity", ".capacity_wh", func(p *Params) { p.Loadpoints[0].CapacityWh = math.NaN() }},
		{"too few levels", ".levels", func(p *Params) { p.Loadpoints[0].Levels = 1 }},
		{"invalid bounds", "SoC bounds", func(p *Params) { p.Loadpoints[0].SoCMin, p.Loadpoints[0].SoCMax = 0.6, 0.50 }},
		{"nan bound", "SoC bounds", func(p *Params) { p.Loadpoints[0].SoCMin = math.NaN() }},
		{"initial outside bounds", "initial_soc", func(p *Params) { p.Loadpoints[0].InitialSoC = 1.01 }},
		{"initial below operating minimum", "initial_soc", func(p *Params) {
			p.Loadpoints[0].SoCMin, p.Loadpoints[0].InitialSoC = 0.3, 0.20
		}},
		{"initial above operating maximum", "initial_soc", func(p *Params) {
			p.Loadpoints[0].SoCMax, p.Loadpoints[0].InitialSoC = 0.1, 0.20
		}},
		{"nan initial", "initial_soc", func(p *Params) { p.Loadpoints[0].InitialSoC = math.NaN() }},
		{"target outside bounds", "target_soc", func(p *Params) { p.Loadpoints[0].TargetSoC = 1.01 }},
		{"target below operating minimum", "target_soc", func(p *Params) {
			p.Loadpoints[0].SoCMin, p.Loadpoints[0].InitialSoC, p.Loadpoints[0].TargetSoC = 0.3, 0.40, 0.20
		}},
		{"target above operating maximum", "target_soc", func(p *Params) {
			p.Loadpoints[0].SoCMax, p.Loadpoints[0].TargetSoC = 0.7, 0.80
		}},
		{"infinite target", "target_soc", func(p *Params) { p.Loadpoints[0].TargetSoC = math.Inf(1) }},
		{"target with negative deadline", "target_slot_idx", func(p *Params) { p.Loadpoints[0].TargetSlotIdx = -1 }},
		{"negative max power", ".max_charge_w", func(p *Params) { p.Loadpoints[0].MaxChargeW = -1 }},
		{"infinite max power", ".max_charge_w", func(p *Params) { p.Loadpoints[0].MaxChargeW = math.Inf(1) }},
		{"negative efficiency", ".charge_efficiency", func(p *Params) { p.Loadpoints[0].ChargeEfficiency = -0.1 }},
		{"nan efficiency", ".charge_efficiency", func(p *Params) { p.Loadpoints[0].ChargeEfficiency = math.NaN() }},
		{"high efficiency", ".charge_efficiency", func(p *Params) { p.Loadpoints[0].ChargeEfficiency = 1.01 }},
		{"nan step", "allowed_steps", func(p *Params) { p.Loadpoints[0].AllowedStepsW = []float64{0, math.NaN()} }},
		{"negative step", "allowed_steps", func(p *Params) { p.Loadpoints[0].AllowedStepsW = []float64{0, -1} }},
		{"step above max", "exceeds max_charge_w", func(p *Params) { p.Loadpoints[0].AllowedStepsW = []float64{0, 12000} }},
		{"fallback different physics", "fallback must match", func(p *Params) {
			fallback := *p.Loadpoint
			fallback.MaxChargeW = 12000
			p.Loadpoint = &fallback
		}},
		{"fallback invalid physics", "loadpoint fallback", func(p *Params) {
			fallback := *p.Loadpoint
			fallback.InitialSoC = math.NaN()
			p.Loadpoint = &fallback
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlanningParams()
			lp := validPlanningLoadpoint()
			p.Loadpoints = []*LoadpointSpec{lp}
			p.Loadpoint = lp
			tc.mutate(&p)
			err := validatePlanningParams(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}

	p := validPlanningStorageParams()
	lp := validPlanningLoadpoint()
	lp.ID = p.Storages[0].ID
	p.Loadpoints = []*LoadpointSpec{lp}
	p.Loadpoint = lp
	if err := validatePlanningParams(p); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("cross-asset duplicate error = %v", err)
	}
}

func TestValidatePlanningSlotsAndFleet(t *testing.T) {
	validSlot := Slot{StartMs: 1, LenMin: 15, PriceOre: -100, SpotOre: -200, PVW: -500, LoadW: 1000, Confidence: 1}
	if err := validatePlanningSlots([]Slot{validSlot}); err != nil {
		t.Fatalf("valid slot rejected: %v", err)
	}
	if err := validateBatteryFleetMembers([]BatteryFleetMember{{
		Driver: "battery", CapacityWh: 10000, MaxChargeW: 0, MaxDischargeW: 0,
	}}); err != nil {
		t.Fatalf("valid disabled fleet member rejected: %v", err)
	}

	slotTests := []struct {
		name   string
		want   string
		mutate func(*Slot)
	}{
		{"nan price", "price_ore", func(s *Slot) { s.PriceOre = math.NaN() }},
		{"infinite spot", "spot_ore", func(s *Slot) { s.SpotOre = math.Inf(1) }},
		{"positive pv", "pv_w", func(s *Slot) { s.PVW = 1 }},
		{"negative load", "load_w", func(s *Slot) { s.LoadW = -1 }},
		{"zero confidence", "confidence", func(s *Slot) { s.Confidence = 0 }},
		{"high confidence", "confidence", func(s *Slot) { s.Confidence = 1.1 }},
		{"negative import limit", "grid limits", func(s *Slot) { s.Limits.MaxImportW = -1 }},
		{"nan export limit", "max_export_w", func(s *Slot) { s.Limits.MaxExportW = math.NaN() }},
	}
	for _, tc := range slotTests {
		t.Run("slot "+tc.name, func(t *testing.T) {
			slot := validSlot
			tc.mutate(&slot)
			err := validatePlanningSlots([]Slot{slot})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}

	fleetTests := []struct {
		name  string
		fleet []BatteryFleetMember
		want  string
	}{
		{"empty driver", []BatteryFleetMember{{CapacityWh: 1}}, ".driver"},
		{"duplicate driver", []BatteryFleetMember{{Driver: "a", CapacityWh: 1}, {Driver: "a", CapacityWh: 1}}, "duplicated"},
		{"zero capacity", []BatteryFleetMember{{Driver: "a", CapacityWh: 0}}, ".capacity_wh"},
		{"nan capacity", []BatteryFleetMember{{Driver: "a", CapacityWh: math.NaN()}}, ".capacity_wh"},
		{"negative charge", []BatteryFleetMember{{Driver: "a", CapacityWh: 1, MaxChargeW: -1}}, ".max_charge_w"},
		{"infinite discharge", []BatteryFleetMember{{Driver: "a", CapacityWh: 1, MaxDischargeW: math.Inf(1)}}, ".max_discharge_w"},
	}
	for _, tc := range fleetTests {
		t.Run("fleet "+tc.name, func(t *testing.T) {
			err := validateBatteryFleetMembers(tc.fleet)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want path %q", err, tc.want)
			}
		})
	}
}

type physicsGateCountingOptimizer struct {
	calls atomic.Int32
}

func (o *physicsGateCountingOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls.Add(1)
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{Engine: "test", Backend: "counting", Status: "optimal"}
	return plan, nil
}

func (*physicsGateCountingOptimizer) Close() error { return nil }

type physicsGateRecoveryOptimizer struct {
	calls atomic.Int32
}

func (o *physicsGateRecoveryOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls.Add(1)
	plan := Plan{
		GeneratedAtMs: time.Now().UnixMilli(), Mode: p.Mode,
		HorizonSlots: len(slots), CapacityWh: p.CapacityWh,
		InitialSoC: p.InitialSoC,
		Actions:    make([]Action, len(slots)),
		Solver:     &SolverInfo{Engine: "test", Backend: "recovery", Status: "optimal"},
	}
	for i, slot := range slots {
		gridW := slot.LoadW + slot.PVW
		cost := SlotGridCostOre(slot, gridW*slot.DurationHours()/1000, p)
		plan.Actions[i] = Action{
			SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin, ExecutionStartMs: slot.ExecutionStartMs,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre,
			PVW: slot.PVW, LoadW: slot.LoadW, Confidence: slot.Confidence,
			GridW: gridW, SoC: p.InitialSoC, CostOre: cost,
		}
		if len(p.Storages) > 0 {
			plan.Actions[i].StoragePowerW = make(map[string]float64)
			plan.Actions[i].StorageEnergyWh = make(map[string]float64)
			for _, b := range p.Storages {
				plan.Actions[i].StoragePowerW[b.ID] = 0
				plan.Actions[i].StorageEnergyWh[b.ID] = b.InitialEnergyWh
			}
		}
		plan.TotalCostOre += cost
	}
	return plan, nil
}

func (*physicsGateRecoveryOptimizer) Close() error { return nil }

type physicsGateFailingOptimizer struct {
	calls atomic.Int32
}

func (o *physicsGateFailingOptimizer) Optimize(context.Context, []Slot, Params) (Plan, error) {
	o.calls.Add(1)
	return Plan{}, errors.New("primary unavailable")
}

func (*physicsGateFailingOptimizer) Close() error { return nil }

func configurePhysicsGateFleet(svc *Service, firstSoC, secondSoC float64) {
	svc.Tele = telemetry.NewStore()
	svc.BatteryFleet = []BatteryFleetMember{
		{Driver: "battery-a", CapacityWh: 5000, MaxChargeW: 1500, MaxDischargeW: 1500},
		{Driver: "battery-b", CapacityWh: 5000, MaxChargeW: 1500, MaxDischargeW: 1500},
	}
	for i, socPct := range []float64{firstSoC, secondSoC} {
		driver := svc.BatteryFleet[i].Driver
		soc := socPct // 0–1
		svc.Tele.Update(driver, telemetry.DerBattery, 0, &soc, nil)
		svc.Tele.DriverHealthMut(driver).RecordSuccess()
	}
}

func TestReplanRejectsInvalidPhysicsBeforeSolver(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Service)
	}{
		{"unknown mode", func(s *Service) { s.Defaults.Mode = "fast" }},
		{"nan capacity", func(s *Service) { s.Defaults.CapacityWh = math.NaN() }},
		{"efficiency above one", func(s *Service) { s.Defaults.ChargeEfficiency = 1.01 }},
		{"reversed soc bounds", func(s *Service) { s.Defaults.SoCMin = 0.96 }},
		{"negative power", func(s *Service) { s.Defaults.MaxDischargeW = -1 }},
		{"negative fuse limit", func(s *Service) { s.FuseMaxW = -1 }},
		{"nan fuse limit", func(s *Service) { s.FuseMaxW = math.NaN() }},
		{"infinite fuse limit", func(s *Service) { s.FuseMaxW = math.Inf(1) }},
		{"negative export limit", func(s *Service) { s.FuseMaxW, s.MaxExportW = 11000, -1 }},
		{"nan export limit", func(s *Service) { s.FuseMaxW, s.MaxExportW = 11000, math.NaN() }},
		{"infinite export limit", func(s *Service) { s.FuseMaxW, s.MaxExportW = 11000, math.Inf(1) }},
		{"nan pv uncertainty", func(s *Service) {
			s.PVUncertaintyW = func() float64 { return math.NaN() }
		}},
		{"negative pv uncertainty", func(s *Service) {
			s.PVUncertaintyW = func() float64 { return -1 }
		}},
		{"invalid plugged loadpoint", func(s *Service) {
			s.Loadpoints = func(int) []*LoadpointSpec {
				return []*LoadpointSpec{{ID: "garage", PluggedIn: true, CapacityWh: 0, Levels: 1}}
			}
		}},
		{"loadpoint initial outside operating bounds", func(s *Service) {
			s.Loadpoints = func(int) []*LoadpointSpec {
				lp := validPlanningLoadpoint()
				lp.SoCMin, lp.InitialSoC = 0.3, 20
				return []*LoadpointSpec{lp}
			}
		}},
		{"loadpoint target outside operating bounds", func(s *Service) {
			s.Loadpoints = func(int) []*LoadpointSpec {
				lp := validPlanningLoadpoint()
				lp.SoCMax, lp.TargetSoC = 0.7, 80
				return []*LoadpointSpec{lp}
			}
		}},
		{"loadpoint target with negative deadline", func(s *Service) {
			s.Loadpoints = func(int) []*LoadpointSpec {
				lp := validPlanningLoadpoint()
				lp.TargetSlotIdx = -1
				return []*LoadpointSpec{lp}
			}
		}},
		{"stale loadpoint fallback", func(s *Service) {
			primary := validPlanningLoadpoint()
			fallback := *primary
			fallback.MaxChargeW = 12000
			s.Defaults.Loadpoints = []*LoadpointSpec{primary}
			s.Defaults.Loadpoint = &fallback
		}},
		{"duplicate fleet", func(s *Service) {
			s.Tele = telemetry.NewStore()
			soc := 0.5
			s.Tele.Update("battery", telemetry.DerBattery, 0, &soc, nil)
			s.Tele.DriverHealthMut("battery").RecordSuccess()
			s.BatteryFleet = []BatteryFleetMember{
				{Driver: "battery", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
				{Driver: "battery", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			now := time.Now().UTC().Truncate(time.Minute)
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
				SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
			}}); err != nil {
				t.Fatal(err)
			}

			optimizer := &physicsGateCountingOptimizer{}
			svc := New(st, nil, "SE3", Params{
				Mode: ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
				CapacityWh: 10000, SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
				MaxChargeW: 3000, MaxDischargeW: 3000,
				ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
			})
			svc.BaseLoad = 500
			svc.Optimizer = optimizer
			var idCalls, saveCalls atomic.Int32
			svc.decisionIDFactory = func() string {
				idCalls.Add(1)
				return "00000000-0000-4000-8000-000000000099"
			}
			svc.SaveDiag = func(*Diagnostic, string) error {
				saveCalls.Add(1)
				return nil
			}

			accepted := svc.Replan(context.Background())
			if accepted == nil || accepted.DecisionID == "" {
				t.Fatalf("valid baseline plan = %+v", accepted)
			}
			tc.mutate(svc)
			got := svc.Replan(context.Background())
			if got != accepted || svc.Latest() != accepted {
				t.Fatalf("invalid inputs replaced prior plan: got=%p accepted=%p latest=%p", got, accepted, svc.Latest())
			}
			if optimizer.calls.Load() != 1 || idCalls.Load() != 1 || saveCalls.Load() != 1 {
				t.Fatalf("calls after rejection: optimizer=%d id=%d save=%d, want 1/1/1",
					optimizer.calls.Load(), idCalls.Load(), saveCalls.Load())
			}
			if d := svc.Diagnose(); d == nil || d.DecisionID != accepted.DecisionID {
				t.Fatalf("diagnostic changed after rejected inputs: %+v", d)
			}
		})
	}
}

func TestReplanSanitizesBadLoadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		load float64
	}{
		{"negative", -1},
		{"nan", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			now := time.Now().UTC().Truncate(time.Minute)
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
				SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
			}}); err != nil {
				t.Fatal(err)
			}
			svc := New(st, nil, "SE3", Params{
				Mode: ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
				CapacityWh: 10000, SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
				MaxChargeW: 3000, MaxDischargeW: 3000,
				ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
			})
			svc.LoadMaxW = 11000
			svc.Load = func(time.Time) float64 { return tc.load }
			plan := svc.Replan(context.Background())
			if plan == nil || len(plan.Actions) == 0 {
				t.Fatalf("sanitized load must still produce a plan, got %+v", plan)
			}
			if plan.Actions[0].LoadW != 0 {
				t.Fatalf("bad load input must floor at 0 W, got %v", plan.Actions[0].LoadW)
			}
		})
	}
}

func TestReplanRejectsInvalidPhysicsBeforeGoDP(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", validPlanningParams())
	svc.BaseLoad = 500
	var idCalls, saveCalls atomic.Int32
	svc.decisionIDFactory = func() string {
		idCalls.Add(1)
		return "00000000-0000-4000-8000-000000000097"
	}
	svc.SaveDiag = func(*Diagnostic, string) error {
		saveCalls.Add(1)
		return nil
	}
	accepted := svc.Replan(context.Background())
	if accepted == nil {
		t.Fatal("valid Go-DP baseline returned nil")
	}
	svc.Defaults.DischargeEfficiency = 1.01
	if got := svc.Replan(context.Background()); got != accepted || svc.Latest() != accepted {
		t.Fatalf("invalid Go-DP inputs replaced prior plan: got=%p accepted=%p latest=%p", got, accepted, svc.Latest())
	}
	svc.Defaults.DischargeEfficiency = validPlanningParams().DischargeEfficiency
	svc.FuseMaxW, svc.MaxExportW = 11000, math.NaN()
	if got := svc.Replan(context.Background()); got != accepted || svc.Latest() != accepted {
		t.Fatalf("invalid Go-DP grid limits replaced prior plan: got=%p accepted=%p latest=%p", got, accepted, svc.Latest())
	}
	if idCalls.Load() != 1 || saveCalls.Load() != 1 {
		t.Fatalf("calls after rejection: id=%d save=%d, want 1/1", idCalls.Load(), saveCalls.Load())
	}
}

func TestReplanRecoveryUsesExternalPlannerWithoutDPDerivedResults(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Service)
	}{
		{"aggregate recovery", func(svc *Service) { svc.Defaults.InitialSoC = 0.05 }},
		{"storage member recovery", func(svc *Service) { configurePhysicsGateFleet(svc, 0.05, 0.55) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			now := time.Now().UTC().Truncate(time.Minute)
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
				SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
			}}); err != nil {
				t.Fatal(err)
			}

			p := validPlanningParams()
			p.Mode = ModeArbitrage
			optimizer := &physicsGateRecoveryOptimizer{}
			svc := New(st, nil, "SE3", p)
			svc.BaseLoad = 500
			svc.Optimizer = optimizer
			tc.setup(svc)
			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver == nil || plan.Solver.Backend != "recovery" {
				t.Fatalf("external recovery plan = %+v", plan)
			}
			if optimizer.calls.Load() != 1 {
				t.Fatalf("external optimizer calls = %d, want 1", optimizer.calls.Load())
			}
			if plan.DPEvaluationShadow != nil || plan.DPShadow != nil || plan.Baselines != nil {
				t.Fatalf("DP-derived results must be absent during recovery: evaluation=%+v downside=%+v baselines=%+v",
					plan.DPEvaluationShadow, plan.DPShadow, plan.Baselines)
			}
		})
	}
}

func TestReplanRecoveryKeepsPreviousPlanWhenDPWouldBeRequired(t *testing.T) {
	tests := []struct {
		name          string
		baseline      PlanOptimizer
		recovery      PlanOptimizer
		wantFailCalls int32
		setupRecovery func(*Service)
	}{
		// Only the external-champion path still refuses: its operating-band
		// recovery ratchet is what the DP cannot represent, so a failed primary
		// must not silently become a DP plan solved from a snapped SoC. With
		// Core as the configured planner the same state is clamped and planned
		// — see TestCoreChampionClampsOutOfBandSoC.
		{name: "aggregate failed primary", baseline: &physicsGateCountingOptimizer{}, recovery: &physicsGateFailingOptimizer{}, wantFailCalls: 1,
			setupRecovery: func(svc *Service) { svc.Defaults.InitialSoC = 0.05 }},
		{name: "storage member failed primary", baseline: &physicsGateCountingOptimizer{}, recovery: &physicsGateFailingOptimizer{}, wantFailCalls: 1,
			setupRecovery: func(svc *Service) { configurePhysicsGateFleet(svc, 0.05, 0.55) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			now := time.Now().UTC().Truncate(time.Minute)
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
				SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
			}}); err != nil {
				t.Fatal(err)
			}

			p := validPlanningParams()
			svc := New(st, nil, "SE3", p)
			svc.BaseLoad = 500
			svc.Optimizer = tc.baseline
			var idCalls, saveCalls atomic.Int32
			svc.decisionIDFactory = func() string {
				idCalls.Add(1)
				return "00000000-0000-4000-8000-000000000098"
			}
			svc.SaveDiag = func(*Diagnostic, string) error {
				saveCalls.Add(1)
				return nil
			}
			accepted := svc.Replan(context.Background())
			if accepted == nil || accepted.DecisionID == "" {
				t.Fatalf("baseline plan = %+v", accepted)
			}

			tc.setupRecovery(svc)
			svc.Optimizer = tc.recovery
			got := svc.Replan(context.Background())
			if got != accepted || svc.Latest() != accepted {
				t.Fatalf("recovery replaced prior plan through Go DP: got=%p accepted=%p latest=%p",
					got, accepted, svc.Latest())
			}
			if idCalls.Load() != 1 || saveCalls.Load() != 1 {
				t.Fatalf("calls after recovery rejection: id=%d save=%d, want 1/1", idCalls.Load(), saveCalls.Load())
			}
			if failing, ok := tc.recovery.(*physicsGateFailingOptimizer); ok && failing.calls.Load() != tc.wantFailCalls {
				t.Fatalf("failing optimizer calls = %d, want %d", failing.calls.Load(), tc.wantFailCalls)
			}
		})
	}
}

func TestReplanSnapshotsPVUncertaintyOnce(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: now.Add(-5 * time.Minute).UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", validPlanningParams())
	svc.BaseLoad = 500
	svc.PVForecastSafetyK = 1
	svc.Optimizer = &physicsGateCountingOptimizer{}
	var calls, relCalls atomic.Int32
	svc.PVUncertaintyW = func() float64 {
		calls.Add(1)
		return 300
	}
	svc.PVRelativeUncertainty = func() float64 {
		relCalls.Add(1)
		return 0.2
	}
	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("Replan returned nil")
	}
	if calls.Load() != 1 {
		t.Fatalf("PV uncertainty sampled %d times, want exactly 1", calls.Load())
	}
	if relCalls.Load() != 1 {
		t.Fatalf("PV relative uncertainty sampled %d times, want exactly 1", relCalls.Load())
	}
}
