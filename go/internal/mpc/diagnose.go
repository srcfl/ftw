package mpc

import "time"

// DiagnosticSlot joins the per-slot inputs the DP saw with the action
// it chose. One row per horizon slot, indexed from 0 at the slot
// containing "now". The UI renders this as the per-slot explainability
// table so operators can answer "why did the planner charge at 21:00?".
type DiagnosticSlot struct {
	Idx         int   `json:"idx"`
	SlotStartMs int64 `json:"slot_start_ms"`
	SlotEndMs   int64 `json:"slot_end_ms"`
	LenMin      int   `json:"len_min"`

	// Inputs
	PriceOre   float64 `json:"price_ore"`  // consumer total (spot + tariff + VAT)
	SpotOre    float64 `json:"spot_ore"`   // raw spot — used for export revenue
	Confidence float64 `json:"confidence"` // 1.0 = day-ahead, 0.6 = forecast
	PVW        float64 `json:"pv_w"`       // site-signed (≤ 0 when producing)
	LoadW      float64 `json:"load_w"`

	// Outputs
	BatteryW float64 `json:"battery_w"`
	GridW    float64 `json:"grid_w"`
	SoCPct   float64 `json:"soc_pct"`  // SoC at END of slot
	CostOre  float64 `json:"cost_ore"` // raw (un-blended) slot cost
	Reason   string  `json:"reason"`
	EMSMode  string  `json:"ems_mode"`
	PVLimitW float64 `json:"pv_limit_w,omitempty"`

	// EV outputs — present only when the plan included a loadpoint.
	// `omitempty` + the web renderer's `lpActive` gate mean plans
	// without an EV look identical to the pre-loadpoint diagnostic
	// shape; plans WITH an EV surface two extra columns so the
	// LOAD / BATTERY math in the table is actually complete. Before
	// these fields were plumbed, operators saw `BATTERY -5.6 kW`
	// against `LOAD 1.6 kW` and reasonably assumed the battery was
	// exporting — reality was `LOAD 1.6 + EV 4.0 = 5.6 kW covered`,
	// grid ≈ 0. See issue #174.
	LoadpointW      float64 `json:"loadpoint_w,omitempty"`
	LoadpointSoCPct float64 `json:"loadpoint_soc_pct,omitempty"`
}

// DiagnosticParams is a JSON-friendly subset of the Params struct —
// enough for operators to verify the DP was parameterized correctly
// without pulling the whole internal struct.
type DiagnosticParams struct {
	Mode                Mode     `json:"mode"`
	InitialSoCPct       float64  `json:"initial_soc_pct"`
	SoCMinPct                    float64 `json:"soc_min_pct"`
	SoCMaxPct                    float64 `json:"soc_max_pct"`
	PVChargeBonusOreKwh          float64 `json:"pv_charge_bonus_ore_kwh,omitempty"`
	SoCLevels                    int     `json:"soc_levels"`
	ActionLevels        int      `json:"action_levels"`
	MaxChargeW          float64  `json:"max_charge_w"`
	MaxDischargeW       float64  `json:"max_discharge_w"`
	ChargeEfficiency    float64  `json:"charge_efficiency"`
	DischargeEfficiency float64  `json:"discharge_efficiency"`
	CapacityWh          float64  `json:"capacity_wh"`
	TerminalSoCPrice    float64  `json:"terminal_soc_price_ore_kwh"`
	ExportBonusOreKwh   float64  `json:"export_bonus_ore_kwh"`
	ExportFeeOreKwh     float64  `json:"export_fee_ore_kwh"`
	ExportFloorOreKwh   *float64 `json:"export_floor_ore_kwh,omitempty"`
}

// Diagnostic is the full post-mortem of the most recent Optimize call.
// Returned by Service.Diagnose for the /api/mpc/diagnose endpoint.
type Diagnostic struct {
	ComputedAtMs   int64            `json:"computed_at_ms"`
	Zone           string           `json:"zone"`
	Horizon        int              `json:"horizon_slots"`
	TotalCostOre   float64          `json:"total_cost_ore"`
	Params         DiagnosticParams `json:"params"`
	Slots          []DiagnosticSlot `json:"slots"`
	LoadpointID    string           `json:"loadpoint_id,omitempty"`
	LastReplanAtMs int64            `json:"last_replan_at_ms"`
	LastReason     string           `json:"last_reason"`
}

// Diagnose returns the inputs + outputs of the most recent Optimize
// call, joined per slot. Returns nil until the first successful replan.
//
// The shape matches what the UI renders in the planner inspector so
// operators can audit each slot: "what did the DP see, what did it
// decide, and why". The per-slot `Reason` string already explains the
// decision class; the adjacent inputs show whether the decision was
// grounded in a real day-ahead price (`confidence == 1.0`) or a
// forecasted one (`confidence == 0.6`).
func (s *Service) Diagnose() *Diagnostic {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil || len(s.lastSlots) == 0 {
		return nil
	}
	return buildDiagnostic(s.last, s.lastSlots, s.lastParams, s.Zone,
		s.lastReplanAt.UnixMilli(), s.lastReason)
}

// buildDiagnostic assembles a Diagnostic from explicit inputs — no
// access to Service state. Used by Diagnose() (holds the read lock
// while calling) and by replan() which passes its locally-captured
// plan/slots/params to guarantee the persisted snapshot + reason are
// atomically paired even when a concurrent replan swaps s.last mid-
// flight.
func buildDiagnostic(plan *Plan, slots []Slot, p Params, zone string,
	replanAtMs int64, reason string) *Diagnostic {
	if plan == nil || len(slots) == 0 {
		return nil
	}
	n := len(slots)
	if len(plan.Actions) < n {
		n = len(plan.Actions)
	}
	out := make([]DiagnosticSlot, n)
	for i := 0; i < n; i++ {
		slot := slots[i]
		action := plan.Actions[i]
		out[i] = DiagnosticSlot{
			Idx:             i,
			SlotStartMs:     slot.StartMs,
			SlotEndMs:       slot.StartMs + int64(slot.LenMin)*60*1000,
			LenMin:          slot.LenMin,
			PriceOre:        slot.PriceOre,
			SpotOre:         slot.SpotOre,
			Confidence:      slot.Confidence,
			PVW:             slot.PVW,
			LoadW:           slot.LoadW,
			BatteryW:        action.BatteryW,
			GridW:           action.GridW,
			SoCPct:          action.SoCPct,
			CostOre:         action.CostOre,
			Reason:          action.Reason,
			EMSMode:         action.EMSMode,
			PVLimitW:        action.PVLimitW,
			LoadpointW:      action.LoadpointW,
			LoadpointSoCPct: action.LoadpointSoCPct,
		}
	}
	loadpointID := ""
	if p.Loadpoint != nil {
		loadpointID = p.Loadpoint.ID
	}
	return &Diagnostic{
		ComputedAtMs: plan.GeneratedAtMs,
		Zone:         zone,
		Horizon:      plan.HorizonSlots,
		TotalCostOre: plan.TotalCostOre,
		Params: DiagnosticParams{
			Mode:                         p.Mode,
			InitialSoCPct:                p.InitialSoCPct,
			SoCMinPct:                    p.SoCMinPct,
			SoCMaxPct:                    p.SoCMaxPct,
			PVChargeBonusOreKwh:          p.PVChargeBonusOreKwh,
			SoCLevels:                    p.SoCLevels,
			ActionLevels:                 p.ActionLevels,
			MaxChargeW:                   p.MaxChargeW,
			MaxDischargeW:                p.MaxDischargeW,
			ChargeEfficiency:             p.ChargeEfficiency,
			DischargeEfficiency:          p.DischargeEfficiency,
			CapacityWh:                   p.CapacityWh,
			TerminalSoCPrice:             p.TerminalSoCPrice,
			ExportBonusOreKwh:            p.ExportBonusOreKwh,
			ExportFeeOreKwh:              p.ExportFeeOreKwh,
			ExportFloorOreKwh:            p.ExportFloorOreKwh,
		},
		LoadpointID:    loadpointID,
		Slots:          out,
		LastReplanAtMs: replanAtMs,
		LastReason:     reason,
	}
}

// RestoreDiagnostic promotes a persisted diagnostic snapshot back into
// the active in-memory plan cache. Diagnostics are already the exact
// plan+slot JSON the UI uses for time travel; restoring them avoids a
// restart/update gap where Diagnose can show a valid plan from SQLite
// while dispatch sees nil and falls into missing-plan behaviour until
// the next successful replan.
func (s *Service) RestoreDiagnostic(d *Diagnostic, now time.Time, reason string) bool {
	if s == nil || d == nil || len(d.Slots) == 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	plan, slots, params, replanAt, ok := planFromDiagnostic(d)
	if !ok {
		return false
	}
	if now.Sub(time.UnixMilli(plan.GeneratedAtMs)) > MaxPlanAge {
		return false
	}
	nowMs := now.UnixMilli()
	inWindow := false
	for _, a := range plan.Actions {
		endMs := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs >= a.SlotStartMs && nowMs < endMs {
			inWindow = true
			break
		}
	}
	if !inWindow {
		return false
	}
	if d.LoadpointID == "" {
		for _, a := range plan.Actions {
			if a.LoadpointW > 0 {
				return false
			}
		}
	}
	if reason == "" {
		reason = "restored_diagnostic"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Defaults.Mode != "" && params.Mode != "" && params.Mode != s.Defaults.Mode {
		return false
	}
	// Merge fields that exist in the current binary's Defaults but
	// could be missing-or-zero in a persisted snapshot written by an
	// older binary. Without this merge, a deploy that adds a new Params
	// field lets the restored snapshot's zero overwrite the operator's
	// intended default until the next successful replan rebuilds params
	// from s.Defaults.
	if params.PVChargeBonusOreKwh == 0 && s.Defaults.PVChargeBonusOreKwh > 0 {
		params.PVChargeBonusOreKwh = s.Defaults.PVChargeBonusOreKwh
	}
	s.last = plan
	s.lastSlots = slots
	s.lastParams = params
	s.lastLoadpointID = d.LoadpointID
	s.lastReplanAt = replanAt
	s.lastReason = reason
	return true
}

func planFromDiagnostic(d *Diagnostic) (*Plan, []Slot, Params, time.Time, bool) {
	generatedAtMs := d.ComputedAtMs
	if generatedAtMs <= 0 {
		generatedAtMs = d.LastReplanAtMs
	}
	if generatedAtMs <= 0 {
		return nil, nil, Params{}, time.Time{}, false
	}
	replanAtMs := d.LastReplanAtMs
	if replanAtMs <= 0 {
		replanAtMs = generatedAtMs
	}
	params := Params{
		Mode:                         d.Params.Mode,
		InitialSoCPct:                d.Params.InitialSoCPct,
		SoCMinPct:                    d.Params.SoCMinPct,
		SoCMaxPct:                    d.Params.SoCMaxPct,
		PVChargeBonusOreKwh:          d.Params.PVChargeBonusOreKwh,
		SoCLevels:                    d.Params.SoCLevels,
		ActionLevels:                 d.Params.ActionLevels,
		MaxChargeW:                   d.Params.MaxChargeW,
		MaxDischargeW:                d.Params.MaxDischargeW,
		ChargeEfficiency:             d.Params.ChargeEfficiency,
		DischargeEfficiency:          d.Params.DischargeEfficiency,
		CapacityWh:                   d.Params.CapacityWh,
		TerminalSoCPrice:             d.Params.TerminalSoCPrice,
		ExportBonusOreKwh:            d.Params.ExportBonusOreKwh,
		ExportFeeOreKwh:              d.Params.ExportFeeOreKwh,
		ExportFloorOreKwh:            d.Params.ExportFloorOreKwh,
	}
	if params.Mode == "" {
		params.Mode = ModeSelfConsumption
	}
	slots := make([]Slot, 0, len(d.Slots))
	actions := make([]Action, 0, len(d.Slots))
	for _, ds := range d.Slots {
		if ds.SlotStartMs <= 0 {
			continue
		}
		lenMin := ds.LenMin
		if lenMin <= 0 && ds.SlotEndMs > ds.SlotStartMs {
			lenMin = int((ds.SlotEndMs - ds.SlotStartMs) / 60000)
		}
		if lenMin <= 0 {
			continue
		}
		slots = append(slots, Slot{
			StartMs:    ds.SlotStartMs,
			LenMin:     lenMin,
			PriceOre:   ds.PriceOre,
			SpotOre:    ds.SpotOre,
			PVW:        ds.PVW,
			LoadW:      ds.LoadW,
			Confidence: ds.Confidence,
		})
		actions = append(actions, Action{
			SlotStartMs:     ds.SlotStartMs,
			SlotLenMin:      lenMin,
			PriceOre:        ds.PriceOre,
			SpotOre:         ds.SpotOre,
			PVW:             ds.PVW,
			LoadW:           ds.LoadW,
			BatteryW:        ds.BatteryW,
			GridW:           ds.GridW,
			SoCPct:          ds.SoCPct,
			CostOre:         ds.CostOre,
			Confidence:      ds.Confidence,
			Reason:          ds.Reason,
			EMSMode:         ds.EMSMode,
			PVLimitW:        ds.PVLimitW,
			LoadpointW:      ds.LoadpointW,
			LoadpointSoCPct: ds.LoadpointSoCPct,
		})
	}
	if len(actions) == 0 {
		return nil, nil, Params{}, time.Time{}, false
	}
	horizon := d.Horizon
	if horizon <= 0 {
		horizon = len(actions)
	}
	plan := &Plan{
		GeneratedAtMs: generatedAtMs,
		Mode:          params.Mode,
		HorizonSlots:  horizon,
		CapacityWh:    params.CapacityWh,
		InitialSoCPct: params.InitialSoCPct,
		TotalCostOre:  d.TotalCostOre,
		Actions:       actions,
	}
	return plan, slots, params, time.UnixMilli(replanAtMs), true
}
