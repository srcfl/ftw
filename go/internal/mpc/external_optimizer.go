package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/optimizercontract"
)

const externalOptimizerSchemaVersion = 1

// PlanOptimizer is the slow-loop mathematical planning boundary. Implementors
// never touch telemetry, state, drivers, or dispatch; they receive an immutable
// planning snapshot and return a candidate Plan for Go-side validation.
type PlanOptimizer interface {
	Optimize(context.Context, []Slot, Params) (Plan, error)
	Close() error
}

// ExternalOptimizerConfig controls a compiled worker. The command is an
// argv array rather than a shell string, so configuration cannot accidentally
// acquire shell expansion semantics.
type ExternalOptimizerConfig struct {
	Command     []string
	Timeout     time.Duration
	IdleTimeout time.Duration
}

// The worker solves one deterministic request. Core has already applied the PV
// risk margin to the slots, so it sends no scenarios and no risk weight.
const (
	optimizerSolver         = "HIGHS"
	optimizerFormulation    = "auto"
	optimizerMIPRelGap      = 0.005
	optimizerCVaRAlpha      = 0.9
	optimizerScenarioPolicy = "shared"
)

// ExternalOptimizer owns one warm JSON-lines worker process. Calls are
// serialized to keep request and response ownership unambiguous. An optional idle timeout releases the worker's solver memory
// between planning bursts.
type ExternalOptimizer struct {
	cfg            ExternalOptimizerConfig
	transport      OptimizerTransport
	timeBudget     func([]Slot, Params) time.Duration
	prepareRequest func(context.Context, *externalRequest, Params) error
}

func NewExternalOptimizer(cfg ExternalOptimizerConfig) (*ExternalOptimizer, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = optimizercontract.DefaultTimeout
	}
	transport, err := NewProcessTransport(ProcessTransportConfig{
		Command: cfg.Command, IdleTimeout: cfg.IdleTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &ExternalOptimizer{cfg: cfg, transport: transport}, nil
}

type externalRequest struct {
	SchemaVersion int                    `json:"schema_version"`
	RequestID     string                 `json:"request_id"`
	Settings      externalSettings       `json:"settings"`
	Slots         []externalSlot         `json:"slots"`
	Storages      []externalStorage      `json:"storages"`
	FlexLoads     []externalFlexLoad     `json:"flex_loads"`
	ThermalLoads  []map[string]any       `json:"thermal_loads"`
	DemandCharges []externalDemandCharge `json:"demand_charges,omitempty"`
}

type externalSettings struct {
	PVCurtailmentMinW        *float64 `json:"pv_curtailment_min_w,omitempty"`
	PVCurtailmentMaxW        *float64 `json:"pv_curtailment_max_w,omitempty"`
	Mode                     Mode     `json:"mode"`
	Solver                   string   `json:"solver"`
	Formulation              string   `json:"formulation"`
	TimeLimitS               float64  `json:"time_limit_s"`
	MIPRelGap                float64  `json:"mip_rel_gap"`
	ExportOrePerKWh          float64  `json:"export_price_per_kwh"`
	ExportBonusOreKwh        float64  `json:"export_bonus_per_kwh"`
	ExportFeeOreKwh          float64  `json:"export_fee_per_kwh"`
	ExportFloorOreKwh        *float64 `json:"export_floor_per_kwh,omitempty"`
	MinArbitrageSpreadOreKwh float64  `json:"min_arbitrage_spread_per_kwh"`
	PVChargeBonusOreKwh      float64  `json:"pv_charge_bonus_per_kwh"`
	CVaRWeight               float64  `json:"cvar_weight"` // always 0, see optimizerCVaRAlpha
	CVaRAlpha                float64  `json:"cvar_alpha"`
	ScenarioPolicy           string   `json:"scenario_policy,omitempty"`
}

type externalSlot struct {
	Confidence       float64 `json:"confidence,omitempty"` // legacy workers require 1; negotiated workers omit it
	ExecutionStartMs int64   `json:"execution_start_ms,omitempty"`
	StartMs          int64   `json:"start_ms"`
	LenMin           int     `json:"len_min"`
	PriceOre         float64 `json:"price_per_kwh"`
	SpotOre          float64 `json:"spot_per_kwh"`
	PVW              float64 `json:"pv_w"`
	LoadW            float64 `json:"load_w"`
	MaxImportW       float64 `json:"max_import_w"`
	MaxExportW       float64 `json:"max_export_w"`
}

type externalStorage struct {
	ID                  string  `json:"id"`
	CapacityWh          float64 `json:"capacity_wh"`
	InitialEnergyWh     float64 `json:"initial_energy_wh"`
	MinEnergyWh         float64 `json:"min_energy_wh"`
	MaxEnergyWh         float64 `json:"max_energy_wh"`
	MaxChargeW          float64 `json:"max_charge_w"`
	MaxDischargeW       float64 `json:"max_discharge_w"`
	ChargeEfficiency    float64 `json:"charge_efficiency"`
	DischargeEfficiency float64 `json:"discharge_efficiency"`
	TerminalPriceOreKWh float64 `json:"terminal_price_per_kwh"`
	CycleCostOreKWh     float64 `json:"cycle_cost_per_kwh"`
}

type externalDemandCharge struct {
	ID         string               `json:"id"`
	PricePerKW float64              `json:"price_per_kw"`
	TopN       int                  `json:"top_n"`
	AlreadyKW  []float64            `json:"already_kw,omitempty"`
	Hours      []externalDemandHour `json:"hours,omitempty"`
}

type externalDemandHour struct {
	StartMs          int64   `json:"start_ms"`
	EndMs            int64   `json:"end_ms"`
	ElapsedImportKWh float64 `json:"elapsed_import_kwh,omitempty"`
	Weight           float64 `json:"weight,omitempty"`
	Group            string  `json:"group,omitempty"`
}

type externalFlexLoad struct {
	ID               string           `json:"id"`
	CapacityWh       float64          `json:"capacity_wh"`
	InitialEnergyWh  float64          `json:"initial_energy_wh"`
	MaxEnergyWh      float64          `json:"max_energy_wh"`
	TargetEnergyWh   float64          `json:"target_energy_wh"`
	TargetSlot       int              `json:"target_slot"`
	ChargeEfficiency float64          `json:"charge_efficiency"`
	MaxChargeW       float64          `json:"max_charge_w"`
	AllowedStepsW    []float64        `json:"allowed_steps_w"`
	SurplusOnly      bool             `json:"surplus_only"`
	NoStorageToLoad  bool             `json:"no_storage_to_load"`
	Charging         *ChargingPeriods `json:"charging,omitempty"`
}

type externalResponse struct {
	SchemaVersion int            `json:"schema_version"`
	RequestID     string         `json:"request_id"`
	OK            bool           `json:"ok"`
	Error         *externalError `json:"error,omitempty"`
	Solver        SolverInfo     `json:"solver"`
	Plan          externalPlan   `json:"plan"`
}

type externalError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type externalPlan struct {
	FlexShortfallWh map[string]float64 `json:"flex_shortfall_wh,omitempty"`
	Mode            Mode               `json:"mode"`
	HorizonSlots    int                `json:"horizon_slots"`
	CapacityWh      float64            `json:"capacity_wh"`
	InitialSoC      float64            `json:"initial_soc_pct"`
	TotalCostOre    float64            `json:"total_cost_ore"`
	Actions         []externalAction   `json:"actions"`
}

type externalAction struct {
	ExecutionStartMs int64              `json:"execution_start_ms,omitempty"`
	SlotStartMs      int64              `json:"slot_start_ms"`
	SlotLenMin       int                `json:"slot_len_min"`
	BatteryW         float64            `json:"battery_w"`
	GridW            float64            `json:"grid_w"`
	SoCPct           float64            `json:"soc_pct"`
	CostOre          float64            `json:"cost_ore"`
	PVLimitW         float64            `json:"pv_limit_w"`
	PVCurtailActive  bool               `json:"pv_curtail_active,omitempty"`
	StoragePowerW    map[string]float64 `json:"storage_power_w"`
	StorageEnergy    map[string]float64 `json:"storage_energy_wh"`
	FlexPowerW       map[string]float64 `json:"flex_power_w"`
	FlexMaxPowerW    map[string]float64 `json:"flex_max_power_w,omitempty"`
	FlexEnergyWh     map[string]float64 `json:"flex_energy_wh"`
	ThermalPowerW    map[string]float64 `json:"thermal_power_w"`
	ThermalState     map[string]float64 `json:"thermal_state"`
}

func (o *ExternalOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	if err := validatePartialSlots(slots); err != nil {
		return Plan{}, err
	}
	request := o.buildRequest(slots, p)
	if o.prepareRequest != nil {
		if err := o.prepareRequest(ctx, &request, p); err != nil {
			return Plan{}, fmt.Errorf("prepare optimizer request: %w", err)
		}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Plan{}, fmt.Errorf("encode optimizer request: %w", err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	line, err := o.transport.RoundTrip(timeoutCtx, payload)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return Plan{}, fmt.Errorf("optimizer timeout after %s: %w", o.cfg.Timeout, err)
		}
		return Plan{}, fmt.Errorf("optimizer transport: %w", err)
	}
	var response externalResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return Plan{}, fmt.Errorf("decode optimizer response: %w", err)
	}
	if response.SchemaVersion != externalOptimizerSchemaVersion {
		return Plan{}, fmt.Errorf("optimizer schema version %d, want %d", response.SchemaVersion, externalOptimizerSchemaVersion)
	}
	if response.RequestID != request.RequestID && !(response.RequestID == "unknown" && !response.OK) {
		return Plan{}, fmt.Errorf("optimizer response request_id %q, want %q", response.RequestID, request.RequestID)
	}
	if !response.OK {
		if response.Error == nil {
			return Plan{}, errors.New("optimizer returned an unspecified error")
		}
		return Plan{}, fmt.Errorf("optimizer %s: %s", response.Error.Code, response.Error.Message)
	}
	if err := validateExternalAssets(request, response.Plan); err != nil {
		return Plan{}, fmt.Errorf("optimizer contract rejected: %w", err)
	}
	plan := response.toPlan(slots, p)
	plan.OptimizerInput = append(json.RawMessage(nil), payload...)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		return Plan{}, fmt.Errorf("optimizer plan rejected: %w", err)
	}
	return plan, nil
}
func (o *ExternalOptimizer) buildRequest(slots []Slot, p Params) externalRequest {
	req := externalRequest{
		SchemaVersion: externalOptimizerSchemaVersion,
		RequestID:     uuid.NewString(),
		Settings: externalSettings{
			Mode:                     p.Mode,
			Solver:                   optimizerSolver,
			Formulation:              optimizerFormulation,
			TimeLimitS:               o.cfg.Timeout.Seconds() * 0.8,
			MIPRelGap:                optimizerMIPRelGap,
			ExportOrePerKWh:          p.ExportOrePerKWh,
			ExportBonusOreKwh:        p.ExportBonusOreKwh,
			ExportFeeOreKwh:          p.ExportFeeOreKwh,
			ExportFloorOreKwh:        p.ExportFloorOreKwh,
			MinArbitrageSpreadOreKwh: p.MinArbitrageSpreadOreKwh,
			PVChargeBonusOreKwh:      p.PVChargeBonusOreKwh,
			CVaRAlpha:                optimizerCVaRAlpha,
			ScenarioPolicy:           optimizerScenarioPolicy,
		},
		Slots:        make([]externalSlot, len(slots)),
		Storages:     []externalStorage{},
		FlexLoads:    []externalFlexLoad{},
		ThermalLoads: []map[string]any{},
	}
	if o.timeBudget != nil {
		req.Settings.TimeLimitS = math.Min(req.Settings.TimeLimitS, o.timeBudget(slots, p).Seconds())
	}
	for i, slot := range slots {
		req.Slots[i] = externalSlot{
			StartMs: slot.StartMs, LenMin: slot.LenMin, ExecutionStartMs: slot.ExecutionStartMs,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre,
			Confidence: 1, PVW: slot.PVW, LoadW: slot.LoadW,
			MaxImportW: slot.Limits.MaxImportW, MaxExportW: slot.Limits.MaxExportW,
		}
	}
	if p.PVCurtailment.Covers(slots) {
		value := p.PVCurtailment.MinW
		req.Settings.PVCurtailmentMinW = &value
		maxValue := p.PVCurtailment.MaxW
		req.Settings.PVCurtailmentMaxW = &maxValue
	}
	if len(p.Storages) > 0 {
		for _, storage := range p.Storages {
			req.Storages = append(req.Storages, externalStorage{
				ID: storage.ID, CapacityWh: storage.CapacityWh,
				InitialEnergyWh: storage.InitialEnergyWh,
				MinEnergyWh:     storage.MinEnergyWh, MaxEnergyWh: storage.MaxEnergyWh,
				MaxChargeW: storage.MaxChargeW, MaxDischargeW: storage.MaxDischargeW,
				ChargeEfficiency:    storage.ChargeEfficiency,
				DischargeEfficiency: storage.DischargeEfficiency,
				TerminalPriceOreKWh: p.TerminalSoCPrice,
			})
		}
	} else if p.CapacityWh > 0 {
		req.Storages = []externalStorage{{
			ID: "home-battery", CapacityWh: p.CapacityWh,
			InitialEnergyWh: p.CapacityWh * p.InitialSoC,
			MinEnergyWh:     p.CapacityWh * p.SoCMin,
			MaxEnergyWh:     p.CapacityWh * p.SoCMax,
			MaxChargeW:      p.MaxChargeW, MaxDischargeW: p.MaxDischargeW,
			ChargeEfficiency: p.ChargeEfficiency, DischargeEfficiency: p.DischargeEfficiency,
			TerminalPriceOreKWh: p.TerminalSoCPrice,
		}}
	}
	// A departure past the horizon becomes a goal at its last slot, as in Core
	// DP. It must not compete with a car that still needs energy before it
	// leaves inside the horizon, so then it gets no deadline.
	loadpoints := p.activeLoadpoints()
	pastHorizon := func(lp *LoadpointSpec) bool { return lp.TargetSoC > 0 && lp.TargetSlotIdx >= len(slots) }
	departsInside := slices.ContainsFunc(loadpoints, func(lp *LoadpointSpec) bool {
		initial, target, _ := lp.requestSoC()
		return lp.deadlineSlot(len(slots)) >= 0 && !pastHorizon(lp) && target > initial
	})
	for _, lp := range loadpoints {
		deadline := lp.deadlineSlot(len(slots))
		if departsInside && pastHorizon(lp) {
			deadline = -1
		}
		steps := lp.normalizedSteps()
		efficiency := lp.ChargeEfficiency
		if efficiency <= 0 {
			efficiency = 0.9
		}
		initialSoC, targetSoC, maxSoC := lp.requestSoC()
		req.FlexLoads = append(req.FlexLoads, externalFlexLoad{
			ID: lp.ID, CapacityWh: lp.CapacityWh,
			InitialEnergyWh: lp.CapacityWh * initialSoC,
			MaxEnergyWh:     lp.CapacityWh * maxSoC,
			TargetEnergyWh:  lp.CapacityWh * targetSoC,
			TargetSlot:      deadline, ChargeEfficiency: efficiency,
			MaxChargeW: lp.MaxChargeW, AllowedStepsW: steps,
			SurplusOnly: lp.SurplusOnly, NoStorageToLoad: lp.blocksBatteryToEV(),
		})
	}
	for _, charge := range p.DemandCharges {
		wire := externalDemandCharge{
			ID: charge.ID, PricePerKW: charge.PricePerKW, TopN: charge.TopN,
			AlreadyKW: charge.AlreadyKW,
		}
		for _, hour := range charge.Hours {
			wire.Hours = append(wire.Hours, externalDemandHour{
				StartMs: hour.StartMs, EndMs: hour.EndMs, ElapsedImportKWh: hour.ElapsedImportKWh,
				Weight: hour.Weight, Group: hour.Group,
			})
		}
		req.DemandCharges = append(req.DemandCharges, wire)
	}
	return req
}

func (r externalResponse) toPlan(slots []Slot, p Params) Plan {
	plan := Plan{
		GeneratedAtMs: time.Now().UnixMilli(), Mode: p.Mode,
		HorizonSlots: len(slots), CapacityWh: p.CapacityWh,
		InitialSoC: p.InitialSoC, TotalCostOre: r.Plan.TotalCostOre,
		Actions: make([]Action, 0, len(r.Plan.Actions)), Solver: &r.Solver,
		LoadpointShortfallWh: r.Plan.FlexShortfallWh,
	}
	meanPrice := 0.0
	for _, slot := range slots {
		meanPrice += slot.PriceOre
	}
	meanPrice /= float64(len(slots))
	for i, candidate := range r.Plan.Actions {
		if i >= len(slots) {
			break
		}
		slot := slots[i]
		action := Action{
			SlotStartMs: candidate.SlotStartMs, SlotLenMin: candidate.SlotLenMin, ExecutionStartMs: candidate.ExecutionStartMs,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre,
			PVW: slot.PVW, LoadW: slot.LoadW,
			BatteryW: candidate.BatteryW, GridW: candidate.GridW,
			SoC: candidate.SoCPct / 100, CostOre: candidate.CostOre,
			PVLimitW:           candidate.PVLimitW,
			PVCurtailActive:    candidate.PVCurtailActive,
			StoragePowerW:      candidate.StoragePowerW,
			LoadpointMaxPowerW: candidate.FlexMaxPowerW,
			StorageEnergyWh:    candidate.StorageEnergy,
		}
		activeLoadpoints := p.activeLoadpoints()
		if len(activeLoadpoints) > 0 {
			action.LoadpointPowerW = make(map[string]float64, len(activeLoadpoints))
			action.LoadpointSoCByID = make(map[string]float64, len(activeLoadpoints))
			for lpIdx, lp := range activeLoadpoints {
				powerW := candidate.FlexPowerW[lp.ID]
				soc := candidate.FlexEnergyWh[lp.ID] / lp.CapacityWh
				action.LoadpointPowerW[lp.ID] = powerW
				action.LoadpointSoCByID[lp.ID] = soc
				if lpIdx == 0 {
					action.LoadpointW = powerW
					action.LoadpointSoC = soc
				}
			}
		}
		action.Reason = reasonFor(slot, action.BatteryW, action.GridW, meanPrice)
		plan.Actions = append(plan.Actions, action)
	}
	return plan
}

func (o *ExternalOptimizer) Close() error {
	return o.transport.Close()
}

// solverGridLimitToleranceW admits only sub-watt feasibility residue from the
// mathematical optimizer. The Go planner still observes the exact slot limits,
// and dispatch keeps its separate fuse guard.
const solverGridLimitToleranceW = 0.1

// ValidatePlan independently replays a candidate plan against the canonical
// site sign convention and current constraints. Solver output is untrusted at
// this boundary: NaN, stale slot alignment, energy drift, illegal EV steps, or
// mode/grid-limit violations reject the entire plan.
func ValidatePlan(slots []Slot, p Params, plan *Plan) error {
	if err := validatePartialSlots(slots); err != nil {
		return err
	}
	if plan == nil {
		return errors.New("nil plan")
	}
	if len(plan.Actions) != len(slots) {
		return fmt.Errorf("action count %d, want %d", len(plan.Actions), len(slots))
	}
	if len(slots) == 0 {
		return errors.New("empty plan")
	}
	soc := p.InitialSoC
	storageEnergy := make(map[string]float64, len(p.Storages))
	storageLowerRecovery := make(map[string]float64, len(p.Storages))
	storageUpperRecovery := make(map[string]float64, len(p.Storages))
	for _, storage := range p.Storages {
		storageEnergy[storage.ID] = storage.InitialEnergyWh
		storageLowerRecovery[storage.ID] = math.Max(0, storage.MinEnergyWh-storage.InitialEnergyWh)
		storageUpperRecovery[storage.ID] = math.Max(0, storage.InitialEnergyWh-storage.MaxEnergyWh)
	}
	lowerSoCRecovery := math.Max(0, p.SoCMin-p.InitialSoC)
	upperSoCRecovery := math.Max(0, p.InitialSoC-p.SoCMax)
	activeLoadpoints := p.activeLoadpoints()
	evSoC := make(map[string]float64, len(activeLoadpoints))
	deadlineShortfall := make(map[string]float64)
	for _, lp := range activeLoadpoints {
		evSoC[lp.ID] = lp.InitialSoC
	}
	totalCost := 0.0
	for i, slot := range slots {
		a := plan.Actions[i]
		if err := validateAssetMaps(p, a); err != nil {
			return fmt.Errorf("slot %d: %w", i, err)
		}
		values := []float64{a.BatteryW, a.GridW, a.SoC, a.CostOre, a.LoadpointW, a.LoadpointSoC, a.PVLimitW}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("slot %d contains non-finite output", i)
			}
		}
		if a.SlotStartMs != slot.StartMs || a.SlotLenMin != slot.LenMin || a.ExecutionStartMs != slot.ExecutionStartMs {
			return fmt.Errorf("slot %d timestamp/length mismatch", i)
		}
		if a.BatteryW > p.MaxChargeW+2 || a.BatteryW < -p.MaxDischargeW-2 {
			return fmt.Errorf("slot %d battery_w %.3f exceeds bounds", i, a.BatteryW)
		}
		dtH := slot.DurationHours()
		if len(p.Storages) > 0 && len(a.StoragePowerW) > 0 {
			var totalPowerW, totalEnergyWh float64
			for _, storage := range p.Storages {
				powerW, powerOK := a.StoragePowerW[storage.ID]
				reportedEnergyWh, energyOK := a.StorageEnergyWh[storage.ID]
				if !powerOK || !energyOK {
					return fmt.Errorf("slot %d storage %s output missing", i, storage.ID)
				}
				if math.IsNaN(powerW) || math.IsInf(powerW, 0) || math.IsNaN(reportedEnergyWh) || math.IsInf(reportedEnergyWh, 0) {
					return fmt.Errorf("slot %d storage %s contains non-finite output", i, storage.ID)
				}
				if powerW > storage.MaxChargeW+2 || powerW < -storage.MaxDischargeW-2 {
					return fmt.Errorf("slot %d storage %s power %.3f exceeds bounds", i, storage.ID, powerW)
				}
				energyWh := storageEnergy[storage.ID] + loadpoint.BatteryEnergyDeltaWh(
					powerW, dtH, storage.ChargeEfficiency, storage.DischargeEfficiency)
				energyToleranceWh := math.Max(1, storage.CapacityWh*0.0002)
				if energyWh < -energyToleranceWh || energyWh > storage.CapacityWh+energyToleranceWh || math.Abs(reportedEnergyWh-energyWh) > energyToleranceWh {
					return fmt.Errorf("slot %d storage %s energy %.3f inconsistent with replay %.3f", i, storage.ID, reportedEnergyWh, energyWh)
				}
				lowerRecovery := math.Max(0, storage.MinEnergyWh-energyWh)
				upperRecovery := math.Max(0, energyWh-storage.MaxEnergyWh)
				if lowerRecovery > storageLowerRecovery[storage.ID]+energyToleranceWh || upperRecovery > storageUpperRecovery[storage.ID]+energyToleranceWh {
					return fmt.Errorf("slot %d storage %s worsens operating-bound recovery", i, storage.ID)
				}
				storageLowerRecovery[storage.ID] = math.Min(storageLowerRecovery[storage.ID], lowerRecovery)
				storageUpperRecovery[storage.ID] = math.Min(storageUpperRecovery[storage.ID], upperRecovery)
				storageEnergy[storage.ID] = energyWh
				totalPowerW += powerW
				totalEnergyWh += energyWh
			}
			if math.Abs(a.BatteryW-totalPowerW) > 2 {
				return fmt.Errorf("slot %d aggregate battery_w %.3f, want %.3f", i, a.BatteryW, totalPowerW)
			}
			soc = totalEnergyWh / p.CapacityWh
		} else if p.CapacityWh == 0 {
			// No storage is a real site topology. Its energy and power stay zero.
			soc = 0
		} else {
			// Go DP publishes an aggregate trajectory. Replay that fleet
			// as one battery; per-storage maps are required only when present.
			if p.CapacityWh <= 0 {
				return fmt.Errorf("slot %d capacity_wh must be positive to replay aggregate energy", i)
			}
			soc += loadpoint.BatteryEnergyDeltaWh(
				a.BatteryW, dtH, p.ChargeEfficiency, p.DischargeEfficiency) / p.CapacityWh
		}
		lowerRecovery := math.Max(0, p.SoCMin-soc)
		upperRecovery := math.Max(0, soc-p.SoCMax)
		if lowerRecovery > lowerSoCRecovery+0.0002 || upperRecovery > upperSoCRecovery+0.0002 || math.Abs(a.SoC-soc) > 0.0002 {
			return fmt.Errorf("slot %d SoC %.4f inconsistent with replay %.4f", i, a.SoC, soc)
		}
		lowerSoCRecovery = math.Min(lowerSoCRecovery, lowerRecovery)
		upperSoCRecovery = math.Min(upperSoCRecovery, upperRecovery)
		totalLoadpointW := 0.0
		for lpIdx, lp := range activeLoadpoints {
			powerW := a.LoadpointPowerW[lp.ID]
			reportedSoC := a.LoadpointSoCByID[lp.ID]
			if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
				powerW, reportedSoC = a.LoadpointW, a.LoadpointSoC
			}
			if math.IsNaN(powerW) || math.IsInf(powerW, 0) || math.IsNaN(reportedSoC) || math.IsInf(reportedSoC, 0) {
				return fmt.Errorf("slot %d loadpoint %s contains non-finite output", i, lp.ID)
			}
			steps := lp.normalizedSteps()
			stepW := powerW
			if peak, ok := a.LoadpointMaxPowerW[lp.ID]; ok {
				if !finite(peak) || powerW < 0 || powerW > peak+1e-6 {
					return fmt.Errorf("slot %d invalid EV on-power", i)
				}
				stepW = peak
			}
			if !slices.ContainsFunc(steps, func(step float64) bool { return math.Abs(step-stepW) <= 2 }) {
				return fmt.Errorf("slot %d loadpoint %s power %.3f is not an allowed step", i, lp.ID, powerW)
			}
			eff := lp.ChargeEfficiency
			if eff <= 0 {
				eff = 0.9
			}
			evSoC[lp.ID] += powerW * dtH * eff / lp.CapacityWh
			maxSoC := lp.SoCMax
			if maxSoC <= lp.SoCMin {
				maxSoC = 1
			}
			if evSoC[lp.ID] < -0.0002 || evSoC[lp.ID] > maxSoC+0.0002 {
				return fmt.Errorf("slot %d loadpoint %s energy exceeds capacity", i, lp.ID)
			}
			if math.Abs(reportedSoC-evSoC[lp.ID]) > 0.0002 {
				return fmt.Errorf("slot %d loadpoint %s SoC %.4f inconsistent with replay %.4f", i, lp.ID, reportedSoC, evSoC[lp.ID])
			}
			// Only a departure inside the horizon can be missed; a later one
			// is planned when its prices arrive.
			if lp.TargetSoC > 0 && i == lp.TargetSlotIdx {
				if missing := max(0, lp.TargetSoC-evSoC[lp.ID]) * lp.CapacityWh; missing > lp.departureMissWh() {
					deadlineShortfall[lp.ID] = missing
				}
			}
			totalLoadpointW += powerW
		}
		// Dispatch currently uses a positive limit to activate PV curtailment.
		// A true zero cap cannot execute yet, so it must not become a plan.
		if a.PVCurtailActive && a.PVLimitW == 0 {
			return fmt.Errorf("slot %d active zero PV cap cannot be dispatched", i)
		}
		if a.PVLimitW < 0 || (!a.PVCurtailActive && a.PVLimitW > 0 && a.PVLimitW > -slot.PVW+2) {
			return fmt.Errorf("slot %d pv_limit_w %.3f exceeds forecast generation %.3f", i, a.PVLimitW, -slot.PVW)
		}
		effectivePVW := slot.PVW
		if a.PVCurtailActive {
			// Applied cap, including a true zero. GridW must already
			// include it; this is the optimizer encoding.
			effectivePVW = loadpoint.EffectivePVW(slot.PVW, a.PVLimitW, true)
			wantGridW := loadpoint.GridW(slot.LoadW, effectivePVW, a.BatteryW, totalLoadpointW)
			if math.Abs(a.GridW-wantGridW) > 2 {
				return fmt.Errorf("slot %d grid balance %.3f, want %.3f", i, a.GridW, wantGridW)
			}
		} else {
			// Go DP writes a positive PVLimitW as a dispatch hint and
			// leaves GridW on the uncurtailed identity. An optimizer
			// that applied a positive cap (legacy, no flag) matches
			// the curtailed identity instead.
			uncurtailedGridW := loadpoint.GridW(slot.LoadW, slot.PVW, a.BatteryW, totalLoadpointW)
			if math.Abs(a.GridW-uncurtailedGridW) > 2 {
				if a.PVLimitW > 0 {
					effectivePVW = loadpoint.EffectivePVW(slot.PVW, a.PVLimitW, true)
				}
				wantGridW := loadpoint.GridW(slot.LoadW, effectivePVW, a.BatteryW, totalLoadpointW)
				if math.Abs(a.GridW-wantGridW) > 2 {
					return fmt.Errorf("slot %d grid balance %.3f, want %.3f", i, a.GridW, wantGridW)
				}
			}
		}
		totalSurplusOnlyW := 0.0
		for lpIdx, lp := range activeLoadpoints {
			powerW := a.LoadpointPowerW[lp.ID]
			if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
				powerW = a.LoadpointW
			}
			if lp.SurplusOnly {
				totalSurplusOnlyW += powerW
			}
			if lp.SurplusOnly && surplusOnlyExceedsHousePV(powerW, slot.LoadW, effectivePVW) {
				return fmt.Errorf("slot %d surplus-only loadpoint %s exceeds PV leftover after house load", i, lp.ID)
			}
			if powerW > 0 && a.BatteryW < 0 && a.GridW < -50 {
				return fmt.Errorf("slot %d loadpoint %s charges during battery-driven export", i, lp.ID)
			}
			if lp.blocksBatteryToEV() && loadpoint.BatteryDischargeFeedsEV(a.BatteryW, powerW, slot.LoadW, effectivePVW) {
				return fmt.Errorf("slot %d battery discharge feeds loadpoint %s", i, lp.ID)
			}
		}
		if surplusOnlyExceedsHousePV(totalSurplusOnlyW, slot.LoadW, effectivePVW) {
			return fmt.Errorf("slot %d surplus-only EVs exceed shared leftover PV", i)
		}
		baseGridW := loadpoint.GridW(slot.LoadW, effectivePVW, 0, totalLoadpointW)
		if !modeAllows(p.Mode, baseGridW, a.GridW, a.BatteryW) {
			return fmt.Errorf("slot %d violates mode %s: baseline_grid_w=%.9f grid_w=%.9f battery_w=%.9f",
				i, p.Mode, baseGridW, a.GridW, a.BatteryW)
		}
		if (slot.Limits.MaxImportW > 0 && a.GridW > slot.Limits.MaxImportW+solverGridLimitToleranceW) ||
			(slot.Limits.MaxExportW > 0 && a.GridW < -slot.Limits.MaxExportW-solverGridLimitToleranceW) {
			return fmt.Errorf("slot %d grid_w %.3f violates grid limits", i, a.GridW)
		}
		if err := validateEVPulse(slot, p, a, effectivePVW); err != nil {
			return fmt.Errorf("slot %d: %w", i, err)
		}
		wantCost := reserveSlotCost(slot, p, a)
		if math.Abs(a.CostOre-wantCost) > 0.05 {
			return fmt.Errorf("slot %d cost %.4f, want %.4f", i, a.CostOre, wantCost)
		}
		totalCost += wantCost
	}
	// Derive the departure deficit from replay, not from a worker claim.
	plan.LoadpointShortfallWh = deadlineShortfall
	if math.Abs(plan.TotalCostOre-totalCost) > 0.1 {
		return fmt.Errorf("total cost %.4f, want %.4f", plan.TotalCostOre, totalCost)
	}
	return nil
}
