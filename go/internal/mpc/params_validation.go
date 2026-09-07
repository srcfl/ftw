package mpc

import (
	"fmt"
	"math"
)

func validatePlanningSlots(slots []Slot) error {
	if len(slots) == 0 {
		return fmt.Errorf("slots must be non-empty")
	}
	for i, slot := range slots {
		field := fmt.Sprintf("slots[%d]", i)
		for _, value := range []struct {
			name string
			v    float64
		}{
			{"price_ore", slot.PriceOre},
			{"spot_ore", slot.SpotOre},
			{"pv_w", slot.PVW},
			{"load_w", slot.LoadW},
			{"confidence", slot.Confidence},
			{"max_import_w", slot.Limits.MaxImportW},
			{"max_export_w", slot.Limits.MaxExportW},
		} {
			if !finite(value.v) {
				return fmt.Errorf("%s.%s must be finite", field, value.name)
			}
		}
		if slot.PVW > 0 {
			return fmt.Errorf("%s.pv_w must be non-positive in the site sign convention", field)
		}
		if slot.LoadW < 0 {
			return fmt.Errorf("%s.load_w must be non-negative in the site sign convention", field)
		}
		if slot.Confidence <= 0 || slot.Confidence > 1 {
			return fmt.Errorf("%s.confidence must be within (0, 1]", field)
		}
		if slot.Limits.MaxImportW < 0 || slot.Limits.MaxExportW < 0 {
			return fmt.Errorf("%s grid limits must be non-negative", field)
		}
	}
	return nil
}

// validateServiceGridLimits checks the raw site limits before slot clamping.
// The clamp treats non-positive values as unset and compares export limits to
// the fuse, so invalid values must not be allowed to disappear in that logic.
func validateServiceGridLimits(fuseMaxW, maxExportW float64) error {
	if err := requireNonNegativePlanningValue("fuse_max_w", fuseMaxW); err != nil {
		return err
	}
	return requireNonNegativePlanningValue("max_export_w", maxExportW)
}

func validateBatteryFleetMembers(fleet []BatteryFleetMember) error {
	seen := make(map[string]struct{}, len(fleet))
	for i, battery := range fleet {
		field := fmt.Sprintf("battery_fleet[%d]", i)
		if battery.Driver == "" {
			return fmt.Errorf("%s.driver must be non-empty", field)
		}
		if _, exists := seen[battery.Driver]; exists {
			return fmt.Errorf("%s.driver %q is duplicated", field, battery.Driver)
		}
		seen[battery.Driver] = struct{}{}
		if err := requirePositivePlanningValue(field+".capacity_wh", battery.CapacityWh); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", battery.MaxChargeW); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_discharge_w", battery.MaxDischargeW); err != nil {
			return err
		}
	}
	return nil
}

// planningParamsRequireRecovery marks states the external model represents
// directly, with an operating-band recovery ratchet, and the discrete Go DP
// does not: the DP snaps its initial SoC onto the operating grid, so solving
// from an out-of-band start plans from energy the site does not have (or
// discards energy it does have).
func planningParamsRequireRecovery(p Params) bool {
	if p.CapacityWh == 0 && len(p.Storages) == 0 {
		return false
	}
	if p.InitialSoC < p.SoCMin || p.InitialSoC > p.SoCMax {
		return true
	}
	for _, storage := range p.Storages {
		if storage.InitialEnergyWh < storage.MinEnergyWh || storage.InitialEnergyWh > storage.MaxEnergyWh {
			return true
		}
	}
	return false
}

// clampParamsIntoOperatingBand pulls a battery that has drifted outside
// soc_min…soc_max back onto the nearest band edge so the Core planner can plan
// at all, and records the real reading in InitialSoCUnclamped. It reports
// whether anything moved, and whether the state was recoverable.
//
// Why clamping beats refusing, which is what a reviewer will ask. The clamp
// overstates available energy by at most (soc_min − real)·capacity — on the
// site that reported this, ~1 % ≈ 96 Wh. A plan that then asks for energy the
// pack does not have is stopped by the layers below: the dispatch clamp and
// the driver's own discharge floor, which exist because planner output is
// never sent directly to hardware. Refusing leaves the site executing a stale
// plan for hours and then falling back to live balancing, which is the worse
// failure. That was tolerable only while reaching this path required a missing
// optimizer; Core is the planner now. The field case is a Ferroamp driver
// floor at 0.10 with the pack settling at 0.09 overnight.
//
// Non-finite or physically impossible readings — SoC outside 0..1, storage
// energy outside 0..capacity — are broken telemetry rather than a recoverable
// state, and are refused. Callers keep the previous plan for those.
func clampParamsIntoOperatingBand(p *Params) (clamped, ok bool) {
	if p == nil {
		return false, false
	}
	if !finite(p.InitialSoC) || p.InitialSoC < 0 || p.InitialSoC > 1 {
		return false, false
	}
	if !finite(p.SoCMin) || !finite(p.SoCMax) || p.SoCMin > p.SoCMax || p.CapacityWh <= 0 {
		return false, false
	}
	for i := range p.Storages {
		s := p.Storages[i]
		if !finite(s.InitialEnergyWh) || s.InitialEnergyWh < 0 || s.InitialEnergyWh > s.CapacityWh {
			return false, false
		}
		if !finite(s.MinEnergyWh) || !finite(s.MaxEnergyWh) || s.MinEnergyWh > s.MaxEnergyWh {
			return false, false
		}
	}

	unclamped := p.InitialSoC
	// Clamp each battery first and re-derive the aggregate from the clamped
	// fleet: validateStorageSpecs requires Σ initial_energy_wh to equal
	// capacity × initial_soc, and the Python shadow receives both.
	if len(p.Storages) > 0 {
		totalWh := 0.0
		for i := range p.Storages {
			s := &p.Storages[i]
			s.InitialEnergyWh = math.Min(math.Max(s.InitialEnergyWh, s.MinEnergyWh), s.MaxEnergyWh)
			totalWh += s.InitialEnergyWh
		}
		p.InitialSoC = totalWh / p.CapacityWh
	}
	p.InitialSoC = math.Min(math.Max(p.InitialSoC, p.SoCMin), p.SoCMax)
	if p.InitialSoC == unclamped {
		return false, true
	}
	p.InitialSoCUnclamped = unclamped
	return true, true
}

// validatePlanningParams checks the effective inputs that both planning
// engines receive. Service calls it after live battery and loadpoint state has
// been applied, so an invalid value cannot reach either the external optimizer
// or the Go fallback with different defaulting or failure semantics.
func validatePlanningParams(p Params) error {
	if err := requireNonNegativePlanningValue("pv_curtailment_min_w", p.PVCurtailment.MinW); err != nil {
		return err
	}
	if p.PVCurtailment.MinW > 0 && !p.PVCurtailment.Valid() {
		return fmt.Errorf("pv_curtailment requires identified drivers")
	}
	switch p.Mode {
	case ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage, ModeArbitrage:
	default:
		return fmt.Errorf("unsupported mode %q", p.Mode)
	}

	if p.SoCLevels < 3 {
		return fmt.Errorf("soc_levels must be at least 3, got %d", p.SoCLevels)
	}
	if p.ActionLevels < 3 {
		return fmt.Errorf("action_levels must be at least 3, got %d", p.ActionLevels)
	}
	if err := requireNonNegativePlanningValue("capacity_wh", p.CapacityWh); err != nil {
		return err
	}
	if p.CapacityWh == 0 && (len(p.Storages) != 0 || p.MaxChargeW != 0 || p.MaxDischargeW != 0 || p.InitialSoC != 0) {
		return fmt.Errorf("zero capacity_wh requires no physical storage, battery power or initial energy")
	}
	if !finite(p.SoCMin) || !finite(p.SoCMax) ||
		p.SoCMin < 0 || p.SoCMin >= p.SoCMax || p.SoCMax > 1 {
		return fmt.Errorf("soc bounds must satisfy 0 <= min < max <= 1, got %.6g..%.6g",
			p.SoCMin, p.SoCMax)
	}
	if !finite(p.InitialSoC) || p.InitialSoC < 0 || p.InitialSoC > 1 {
		return fmt.Errorf("initial_soc must be within 0..1, got %.6g", p.InitialSoC)
	}
	if err := requireNonNegativePlanningValue("max_charge_w", p.MaxChargeW); err != nil {
		return err
	}
	if err := requireNonNegativePlanningValue("max_discharge_w", p.MaxDischargeW); err != nil {
		return err
	}
	if err := requirePlanningEfficiency("charge_efficiency", p.ChargeEfficiency, false); err != nil {
		return err
	}
	if err := requirePlanningEfficiency("discharge_efficiency", p.DischargeEfficiency, false); err != nil {
		return err
	}

	for _, value := range []struct {
		name string
		v    float64
	}{
		{"terminal_soc_price", p.TerminalSoCPrice},
		{"export_ore_per_kwh", p.ExportOrePerKWh},
		{"export_bonus_ore_kwh", p.ExportBonusOreKwh},
		{"export_fee_ore_kwh", p.ExportFeeOreKwh},
	} {
		if !finite(value.v) {
			return fmt.Errorf("%s must be finite", value.name)
		}
	}
	if p.ExportFloorOreKwh != nil && !finite(*p.ExportFloorOreKwh) {
		return fmt.Errorf("export_floor_ore_kwh must be finite")
	}
	for _, value := range []struct {
		name string
		v    float64
	}{
		{"pv_charge_bonus_ore_kwh", p.PVChargeBonusOreKwh},
		{"min_arbitrage_spread_ore_kwh", p.MinArbitrageSpreadOreKwh},
		{"pv_uncertainty_w", p.PVUncertaintyW},
		{"pv_relative_uncertainty", p.PVRelativeUncertainty},
		{"pv_forecast_safety_k", p.PVForecastSafetyK},
	} {
		if err := requireNonNegativePlanningValue(value.name, value.v); err != nil {
			return err
		}
	}

	assetIDs := make(map[string]string, len(p.Storages)+len(p.Loadpoints)+1)
	if err := validateStorageSpecs(p, assetIDs); err != nil {
		return err
	}
	storageIDs := make(map[string]string, len(assetIDs))
	for id, field := range assetIDs {
		storageIDs[id] = field
	}
	if err := validateLoadpointSpecs(planningLoadpointSpecs(p), assetIDs); err != nil {
		return err
	}
	activeList := activeLoadpointSpecs(p.Loadpoints)
	if len(p.Loadpoints) > 0 && p.Loadpoint != nil && p.Loadpoint.active() {
		// The external planner consumes Loadpoints while the Go fallback consumes
		// Loadpoint. Validate the fallback on its own so a stale copy cannot hide
		// behind a valid list and reach only the fallback engine.
		if err := validateLoadpointSpecs([]*LoadpointSpec{p.Loadpoint}, storageIDs); err != nil {
			return fmt.Errorf("loadpoint fallback: %w", err)
		}
	}
	if len(activeList) > 0 && !planningLoadpointsEquivalent(p.Loadpoint, activeList[0]) {
		return fmt.Errorf("loadpoint fallback must match first active loadpoint %q", activeList[0].ID)
	}
	return nil
}

func validateStorageSpecs(p Params, assetIDs map[string]string) error {
	var totalCapacityWh, totalInitialWh, totalMinWh, totalMaxWh float64
	var totalChargeW, totalDischargeW float64
	for i, storage := range p.Storages {
		field := fmt.Sprintf("storages[%d]", i)
		if storage.ID == "" {
			return fmt.Errorf("%s.id must be non-empty", field)
		}
		if previous, exists := assetIDs[storage.ID]; exists {
			return fmt.Errorf("%s.id %q duplicates %s", field, storage.ID, previous)
		}
		assetIDs[storage.ID] = field
		if err := requirePositivePlanningValue(field+".capacity_wh", storage.CapacityWh); err != nil {
			return err
		}
		if !finite(storage.MinEnergyWh) || !finite(storage.MaxEnergyWh) ||
			storage.MinEnergyWh < 0 || storage.MinEnergyWh > storage.MaxEnergyWh ||
			storage.MaxEnergyWh > storage.CapacityWh {
			return fmt.Errorf("%s energy bounds must satisfy 0 <= min <= max <= capacity", field)
		}
		// Initial energy outside the operating band is recoverable, but energy
		// outside the physical battery is not.
		if !finite(storage.InitialEnergyWh) || storage.InitialEnergyWh < 0 ||
			storage.InitialEnergyWh > storage.CapacityWh {
			return fmt.Errorf("%s.initial_energy_wh must be within 0..capacity", field)
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", storage.MaxChargeW); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_discharge_w", storage.MaxDischargeW); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".charge_efficiency", storage.ChargeEfficiency, false); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".discharge_efficiency", storage.DischargeEfficiency, false); err != nil {
			return err
		}
		totalCapacityWh += storage.CapacityWh
		totalInitialWh += storage.InitialEnergyWh
		totalMinWh += storage.MinEnergyWh
		totalMaxWh += storage.MaxEnergyWh
		totalChargeW += storage.MaxChargeW
		totalDischargeW += storage.MaxDischargeW
	}
	if len(p.Storages) == 0 {
		return nil
	}

	energyToleranceWh := math.Max(1, p.CapacityWh*0.0002)
	checks := []struct {
		name string
		got  float64
		want float64
		tol  float64
	}{
		{"capacity_wh", totalCapacityWh, p.CapacityWh, 1},
		{"initial_energy_wh", totalInitialWh, p.CapacityWh * p.InitialSoC, energyToleranceWh},
		{"min_energy_wh", totalMinWh, p.CapacityWh * p.SoCMin, energyToleranceWh},
		{"max_energy_wh", totalMaxWh, p.CapacityWh * p.SoCMax, energyToleranceWh},
		{"max_charge_w", totalChargeW, p.MaxChargeW, 2},
		{"max_discharge_w", totalDischargeW, p.MaxDischargeW, 2},
	}
	for _, check := range checks {
		if math.Abs(check.got-check.want) > check.tol {
			return fmt.Errorf("storage aggregate %s %.6g does not match fallback %.6g",
				check.name, check.got, check.want)
		}
	}
	return nil
}

func planningLoadpointSpecs(p Params) []*LoadpointSpec {
	if len(p.Loadpoints) > 0 {
		return p.Loadpoints
	}
	if p.Loadpoint != nil {
		return []*LoadpointSpec{p.Loadpoint}
	}
	return nil
}

func activeLoadpointSpecs(loadpoints []*LoadpointSpec) []*LoadpointSpec {
	active := make([]*LoadpointSpec, 0, len(loadpoints))
	for _, loadpoint := range loadpoints {
		if loadpoint.active() {
			active = append(active, loadpoint)
		}
	}
	return active
}

func planningLoadpointsEquivalent(fallback, primary *LoadpointSpec) bool {
	if fallback == nil || primary == nil || fallback.PluggedIn != primary.PluggedIn ||
		fallback.ID != primary.ID || fallback.Levels != primary.Levels ||
		fallback.TargetSlotIdx != primary.TargetSlotIdx ||
		fallback.SurplusOnly != primary.SurplusOnly ||
		fallback.NoBatteryToEV != primary.NoBatteryToEV {
		return false
	}
	fallbackMin, fallbackMax := planningLoadpointBounds(fallback)
	primaryMin, primaryMax := planningLoadpointBounds(primary)
	for _, values := range [][2]float64{
		{fallback.CapacityWh, primary.CapacityWh},
		{fallbackMin, primaryMin},
		{fallbackMax, primaryMax},
		{fallback.InitialSoC, primary.InitialSoC},
		{fallback.TargetSoC, primary.TargetSoC},
		{fallback.MaxChargeW, primary.MaxChargeW},
		{planningLoadpointEfficiency(fallback), planningLoadpointEfficiency(primary)},
	} {
		if !planningValuesEqual(values[0], values[1]) {
			return false
		}
	}
	fallbackSteps := fallback.normalizedSteps()
	primarySteps := primary.normalizedSteps()
	if len(fallbackSteps) != len(primarySteps) {
		return false
	}
	for i := range fallbackSteps {
		if !planningValuesEqual(fallbackSteps[i], primarySteps[i]) {
			return false
		}
	}
	return true
}

func planningLoadpointBounds(loadpoint *LoadpointSpec) (float64, float64) {
	minSoC, maxSoC := loadpoint.SoCMin, loadpoint.SoCMax
	if minSoC == 0 && maxSoC == 0 {
		maxSoC = 1
	}
	return minSoC, maxSoC
}

func planningLoadpointEfficiency(loadpoint *LoadpointSpec) float64 {
	if loadpoint.ChargeEfficiency == 0 {
		return 0.9
	}
	return loadpoint.ChargeEfficiency
}

func validateLoadpointSpecs(loadpoints []*LoadpointSpec, assetIDs map[string]string) error {
	for i, loadpoint := range loadpoints {
		if loadpoint == nil || !loadpoint.PluggedIn {
			continue
		}
		field := fmt.Sprintf("loadpoints[%d]", i)
		if loadpoint.ID == "" {
			return fmt.Errorf("%s.id must be non-empty", field)
		}
		if previous, exists := assetIDs[loadpoint.ID]; exists {
			return fmt.Errorf("%s.id %q duplicates %s", field, loadpoint.ID, previous)
		}
		assetIDs[loadpoint.ID] = field
		if err := requirePositivePlanningValue(field+".capacity_wh", loadpoint.CapacityWh); err != nil {
			return err
		}
		if loadpoint.Levels < 2 {
			return fmt.Errorf("%s.levels must be at least 2, got %d", field, loadpoint.Levels)
		}
		minSoC, maxSoC := planningLoadpointBounds(loadpoint)
		if !finite(minSoC) || !finite(maxSoC) ||
			minSoC < 0 || minSoC >= maxSoC || maxSoC > 1 {
			return fmt.Errorf("%s SoC bounds must satisfy 0 <= min < max <= 1", field)
		}
		if !finite(loadpoint.InitialSoC) || loadpoint.InitialSoC < minSoC ||
			loadpoint.InitialSoC > maxSoC {
			return fmt.Errorf("%s.initial_soc must be within the loadpoint SoC bounds", field)
		}
		if !finite(loadpoint.TargetSoC) || loadpoint.TargetSoC < 0 ||
			loadpoint.TargetSoC > 1 ||
			(loadpoint.TargetSoC != 0 &&
				(loadpoint.TargetSoC < minSoC || loadpoint.TargetSoC > maxSoC)) {
			return fmt.Errorf("%s.target_soc must be zero or within the loadpoint SoC bounds", field)
		}
		if loadpoint.TargetSoC > 0 && loadpoint.TargetSlotIdx < 0 {
			return fmt.Errorf("%s.target_slot_idx must be non-negative when target_soc is set", field)
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", loadpoint.MaxChargeW); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".charge_efficiency", loadpoint.ChargeEfficiency, true); err != nil {
			return err
		}
		for stepIdx, stepW := range loadpoint.AllowedStepsW {
			if !finite(stepW) || stepW < 0 {
				return fmt.Errorf("%s.allowed_steps_w[%d] must be finite and non-negative", field, stepIdx)
			}
			if loadpoint.MaxChargeW > 0 && stepW > loadpoint.MaxChargeW {
				return fmt.Errorf("%s.allowed_steps_w[%d] exceeds max_charge_w", field, stepIdx)
			}
		}
	}
	return nil
}

func requirePositivePlanningValue(name string, value float64) error {
	if !finite(value) || value <= 0 {
		return fmt.Errorf("%s must be finite and greater than zero", name)
	}
	return nil
}

func requireNonNegativePlanningValue(name string, value float64) error {
	if !finite(value) || value < 0 {
		return fmt.Errorf("%s must be finite and non-negative", name)
	}
	return nil
}

func requirePlanningEfficiency(name string, value float64, allowDefaultZero bool) error {
	if !finite(value) || value < 0 || value > 1 || (!allowDefaultZero && value == 0) {
		if allowDefaultZero {
			return fmt.Errorf("%s must be zero or within (0, 1]", name)
		}
		return fmt.Errorf("%s must be within (0, 1]", name)
	}
	return nil
}

func planningValuesEqual(a, b float64) bool {
	tolerance := math.Max(1e-6, math.Max(math.Abs(a), math.Abs(b))*1e-9)
	return math.Abs(a-b) <= tolerance
}
