package mpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const externalOptimizerSchemaVersion = 1

// PlanOptimizer is the slow-loop mathematical planning boundary. Implementors
// never touch telemetry, state, drivers, or dispatch; they receive an immutable
// planning snapshot and return a candidate Plan for Go-side validation.
type PlanOptimizer interface {
	Optimize(context.Context, []Slot, Params) (Plan, error)
	Close() error
}

// RecourseOptimizer is implemented by optimizers that can solve the same
// immutable planning snapshot with scenario-dependent future decisions. The
// returned plan is diagnostic only; Service never promotes it to dispatch.
type RecourseOptimizer interface {
	OptimizeRecourse(context.Context, []Slot, Params, int) (Plan, error)
}

// MultistageOptimizer exposes the calibrated scenario-tree challenger. Like
// recourse, its result is diagnostic-only and cannot reach dispatch.
type MultistageOptimizer interface {
	OptimizeMultistage(context.Context, []Slot, Params, int) (Plan, error)
}

type MultistageOptimizerConfig struct {
	ScenarioLimit          int
	BranchIntervalSlots    int
	BranchHorizonSlots     int
	MaxBranching           int
	NearHorizonSlots       int
	MidHorizonSlots        int
	MidBlockSlots          int
	FarBlockSlots          int
	ServiceCVaRWeight      *float64
	ServiceCVaRAlpha       float64
	EconomicCVaRWeight     float64
	EconomicCVaRAlpha      float64
	DecompositionThreshold int
	DecompositionMethod    string
	PHMaxIterations        int
	PHRho                  float64
	PHToleranceW           float64
}

// ExternalOptimizerConfig controls the local Python worker. The command is an
// argv array rather than a shell string, so configuration cannot accidentally
// acquire shell expansion semantics.
type ExternalOptimizerConfig struct {
	Command     []string
	ModuleDir   string
	Timeout     time.Duration
	Solver      string
	Formulation string
	MIPRelGap   float64
	CVaRWeight  float64
	CVaRAlpha   float64
	IdleTimeout time.Duration
	Multistage  MultistageOptimizerConfig
}

// ExternalOptimizer owns one warm JSON-lines worker process. Calls are
// serialized because CVXPY problem construction and warm-start state live in
// that process. An optional idle timeout releases the worker's solver memory
// between planning bursts.
type ExternalOptimizer struct {
	cfg ExternalOptimizerConfig

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	waitCh    chan error
	idleTimer *time.Timer
}

func NewExternalOptimizer(cfg ExternalOptimizerConfig) (*ExternalOptimizer, error) {
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return nil, errors.New("optimizer command is empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Solver == "" {
		cfg.Solver = "HIGHS"
	}
	if cfg.Formulation == "" {
		cfg.Formulation = "auto"
	}
	if cfg.MIPRelGap <= 0 {
		cfg.MIPRelGap = 0.005
	}
	if cfg.CVaRAlpha <= 0 || cfg.CVaRAlpha >= 1 {
		cfg.CVaRAlpha = 0.9
	}
	ms := &cfg.Multistage
	if ms.ScenarioLimit <= 0 {
		ms.ScenarioLimit = 12
	}
	if ms.BranchIntervalSlots <= 0 {
		ms.BranchIntervalSlots = 4
	}
	if ms.BranchHorizonSlots <= 0 {
		ms.BranchHorizonSlots = 48
	}
	if ms.MaxBranching <= 0 {
		ms.MaxBranching = 2
	}
	if ms.NearHorizonSlots <= 0 {
		ms.NearHorizonSlots = 16
	}
	if ms.MidHorizonSlots <= 0 {
		ms.MidHorizonSlots = 96
	}
	if ms.MidBlockSlots <= 0 {
		ms.MidBlockSlots = 2
	}
	if ms.FarBlockSlots <= 0 {
		ms.FarBlockSlots = 4
	}
	if ms.ServiceCVaRWeight == nil {
		defaultWeight := 1.0
		ms.ServiceCVaRWeight = &defaultWeight
	}
	if ms.ServiceCVaRAlpha <= 0 || ms.ServiceCVaRAlpha >= 1 {
		ms.ServiceCVaRAlpha = 0.95
	}
	if ms.EconomicCVaRAlpha <= 0 || ms.EconomicCVaRAlpha >= 1 {
		ms.EconomicCVaRAlpha = 0.9
	}
	if ms.DecompositionThreshold <= 0 {
		ms.DecompositionThreshold = 20
	}
	if ms.DecompositionMethod == "" {
		ms.DecompositionMethod = "auto"
	}
	if ms.PHMaxIterations <= 0 {
		ms.PHMaxIterations = 8
	}
	if ms.PHRho <= 0 {
		ms.PHRho = 50
	}
	if ms.PHToleranceW <= 0 {
		ms.PHToleranceW = 5
	}
	return &ExternalOptimizer{cfg: cfg}, nil
}

type externalRequest struct {
	SchemaVersion int                `json:"schema_version"`
	RequestID     string             `json:"request_id"`
	Settings      externalSettings   `json:"settings"`
	Slots         []externalSlot     `json:"slots"`
	Storages      []externalStorage  `json:"storages"`
	FlexLoads     []externalFlexLoad `json:"flex_loads"`
	ThermalLoads  []map[string]any   `json:"thermal_loads"`
	Scenarios     []map[string]any   `json:"scenarios,omitempty"`
}

type externalSettings struct {
	Mode                     Mode     `json:"mode"`
	Solver                   string   `json:"solver"`
	Formulation              string   `json:"formulation"`
	TimeLimitS               float64  `json:"time_limit_s"`
	MIPRelGap                float64  `json:"mip_rel_gap"`
	ExportOrePerKWh          float64  `json:"export_ore_per_kwh"`
	ExportBonusOreKwh        float64  `json:"export_bonus_ore_kwh"`
	ExportFeeOreKwh          float64  `json:"export_fee_ore_kwh"`
	ExportFloorOreKwh        *float64 `json:"export_floor_ore_kwh,omitempty"`
	MinArbitrageSpreadOreKwh float64  `json:"min_arbitrage_spread_ore_kwh"`
	PVChargeBonusOreKwh      float64  `json:"pv_charge_bonus_ore_kwh"`
	CVaRWeight               float64  `json:"cvar_weight"`
	CVaRAlpha                float64  `json:"cvar_alpha"`
	ScenarioPolicy           string   `json:"scenario_policy,omitempty"`
	NonAnticipativeSlots     int      `json:"non_anticipative_slots,omitempty"`
	ScenarioLimit            int      `json:"scenario_limit,omitempty"`
	BranchIntervalSlots      int      `json:"branch_interval_slots,omitempty"`
	BranchHorizonSlots       int      `json:"branch_horizon_slots,omitempty"`
	MaxBranching             int      `json:"max_branching,omitempty"`
	NearHorizonSlots         int      `json:"near_horizon_slots,omitempty"`
	MidHorizonSlots          int      `json:"mid_horizon_slots,omitempty"`
	MidBlockSlots            int      `json:"mid_block_slots,omitempty"`
	FarBlockSlots            int      `json:"far_block_slots,omitempty"`
	ServiceCVaRWeight        float64  `json:"service_cvar_weight,omitempty"`
	ServiceCVaRAlpha         float64  `json:"service_cvar_alpha,omitempty"`
	EconomicCVaRWeight       float64  `json:"economic_cvar_weight,omitempty"`
	EconomicCVaRAlpha        float64  `json:"economic_cvar_alpha,omitempty"`
	DecompositionThreshold   int      `json:"decomposition_threshold,omitempty"`
	DecompositionMethod      string   `json:"decomposition_method,omitempty"`
	PHMaxIterations          int      `json:"ph_max_iterations,omitempty"`
	PHRho                    float64  `json:"ph_rho,omitempty"`
	PHToleranceW             float64  `json:"ph_tolerance_w,omitempty"`
}

type externalSlot struct {
	StartMs    int64   `json:"start_ms"`
	LenMin     int     `json:"len_min"`
	PriceOre   float64 `json:"price_ore"`
	SpotOre    float64 `json:"spot_ore"`
	Confidence float64 `json:"confidence"`
	PVW        float64 `json:"pv_w"`
	LoadW      float64 `json:"load_w"`
	MaxImportW float64 `json:"max_import_w"`
	MaxExportW float64 `json:"max_export_w"`
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
	TerminalPriceOreKWh float64 `json:"terminal_price_ore_kwh"`
	CycleCostOreKWh     float64 `json:"cycle_cost_ore_kwh"`
}

type externalFlexLoad struct {
	ID               string    `json:"id"`
	CapacityWh       float64   `json:"capacity_wh"`
	InitialEnergyWh  float64   `json:"initial_energy_wh"`
	MaxEnergyWh      float64   `json:"max_energy_wh"`
	TargetEnergyWh   float64   `json:"target_energy_wh"`
	TargetSlot       int       `json:"target_slot"`
	ChargeEfficiency float64   `json:"charge_efficiency"`
	MaxChargeW       float64   `json:"max_charge_w"`
	AllowedStepsW    []float64 `json:"allowed_steps_w"`
	SurplusOnly      bool      `json:"surplus_only"`
	NoStorageToLoad  bool      `json:"no_storage_to_load"`
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
	Mode          Mode             `json:"mode"`
	HorizonSlots  int              `json:"horizon_slots"`
	CapacityWh    float64          `json:"capacity_wh"`
	InitialSoCPct float64          `json:"initial_soc_pct"`
	TotalCostOre  float64          `json:"total_cost_ore"`
	Actions       []externalAction `json:"actions"`
}

type externalAction struct {
	SlotStartMs   int64              `json:"slot_start_ms"`
	SlotLenMin    int                `json:"slot_len_min"`
	BatteryW      float64            `json:"battery_w"`
	GridW         float64            `json:"grid_w"`
	SoCPct        float64            `json:"soc_pct"`
	CostOre       float64            `json:"cost_ore"`
	PVLimitW      float64            `json:"pv_limit_w"`
	StoragePowerW map[string]float64 `json:"storage_power_w"`
	StorageEnergy map[string]float64 `json:"storage_energy_wh"`
	FlexPowerW    map[string]float64 `json:"flex_power_w"`
	FlexEnergyWh  map[string]float64 `json:"flex_energy_wh"`
	ThermalPowerW map[string]float64 `json:"thermal_power_w"`
	ThermalState  map[string]float64 `json:"thermal_state"`
}

func (o *ExternalOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	return o.optimize(ctx, slots, p, "shared", 0)
}

// OptimizeRecourse runs the storage recourse challenger in the same warm
// worker as the champion. Calls remain serialized, avoiding a second resident
// CVXPY/HiGHS process on memory-constrained edge hosts.
func (o *ExternalOptimizer) OptimizeRecourse(ctx context.Context, slots []Slot, p Params, nonAnticipativeSlots int) (Plan, error) {
	if nonAnticipativeSlots < 1 {
		nonAnticipativeSlots = 1
	}
	return o.optimize(ctx, slots, p, "recourse", nonAnticipativeSlots)
}

func (o *ExternalOptimizer) OptimizeMultistage(ctx context.Context, slots []Slot, p Params, nonAnticipativeSlots int) (Plan, error) {
	if nonAnticipativeSlots < 1 {
		nonAnticipativeSlots = 1
	}
	return o.optimize(ctx, slots, p, "multistage", nonAnticipativeSlots)
}

func (o *ExternalOptimizer) optimize(ctx context.Context, slots []Slot, p Params, scenarioPolicy string, nonAnticipativeSlots int) (Plan, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancelIdleStopLocked()

	request := o.buildRequest(slots, p)
	request.Settings.ScenarioPolicy = scenarioPolicy
	request.Settings.NonAnticipativeSlots = nonAnticipativeSlots
	if scenarioPolicy == "multistage" {
		ms := o.cfg.Multistage
		request.Settings.ScenarioLimit = ms.ScenarioLimit
		request.Settings.BranchIntervalSlots = ms.BranchIntervalSlots
		request.Settings.BranchHorizonSlots = ms.BranchHorizonSlots
		request.Settings.MaxBranching = ms.MaxBranching
		request.Settings.NearHorizonSlots = ms.NearHorizonSlots
		request.Settings.MidHorizonSlots = ms.MidHorizonSlots
		request.Settings.MidBlockSlots = ms.MidBlockSlots
		request.Settings.FarBlockSlots = ms.FarBlockSlots
		request.Settings.ServiceCVaRWeight = *ms.ServiceCVaRWeight
		request.Settings.ServiceCVaRAlpha = ms.ServiceCVaRAlpha
		request.Settings.EconomicCVaRWeight = ms.EconomicCVaRWeight
		request.Settings.EconomicCVaRAlpha = ms.EconomicCVaRAlpha
		request.Settings.DecompositionThreshold = ms.DecompositionThreshold
		request.Settings.DecompositionMethod = ms.DecompositionMethod
		request.Settings.PHMaxIterations = ms.PHMaxIterations
		request.Settings.PHRho = ms.PHRho
		request.Settings.PHToleranceW = ms.PHToleranceW
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Plan{}, fmt.Errorf("encode optimizer request: %w", err)
	}
	if err := o.ensureStartedLocked(); err != nil {
		return Plan{}, err
	}
	if _, err := o.stdin.Write(append(payload, '\n')); err != nil {
		o.stopLocked()
		return Plan{}, fmt.Errorf("write optimizer request: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	type scanResult struct {
		line []byte
		err  error
	}
	resultCh := make(chan scanResult, 1)
	go func() {
		if !o.scanner.Scan() {
			err := o.scanner.Err()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			resultCh <- scanResult{err: err}
			return
		}
		line := append([]byte(nil), o.scanner.Bytes()...)
		resultCh <- scanResult{line: line}
	}()

	var scanned scanResult
	select {
	case scanned = <-resultCh:
	case <-timeoutCtx.Done():
		o.stopLocked()
		<-resultCh
		return Plan{}, fmt.Errorf("optimizer timeout after %s: %w", o.cfg.Timeout, timeoutCtx.Err())
	}
	if scanned.err != nil {
		o.stopLocked()
		return Plan{}, fmt.Errorf("read optimizer response: %w", scanned.err)
	}
	var response externalResponse
	if err := json.Unmarshal(scanned.line, &response); err != nil {
		o.stopLocked()
		return Plan{}, fmt.Errorf("decode optimizer response: %w", err)
	}
	if response.SchemaVersion != externalOptimizerSchemaVersion {
		o.stopLocked()
		return Plan{}, fmt.Errorf("optimizer schema version %d, want %d", response.SchemaVersion, externalOptimizerSchemaVersion)
	}
	if response.RequestID != request.RequestID && !(response.RequestID == "unknown" && !response.OK) {
		o.stopLocked()
		return Plan{}, fmt.Errorf("optimizer response request_id %q, want %q", response.RequestID, request.RequestID)
	}
	defer o.scheduleIdleStopLocked()
	if !response.OK {
		if response.Error == nil {
			return Plan{}, errors.New("optimizer returned an unspecified error")
		}
		return Plan{}, fmt.Errorf("optimizer %s: %s", response.Error.Code, response.Error.Message)
	}
	plan := response.toPlan(slots, p)
	plan.OptimizerInput = append(json.RawMessage(nil), payload...)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		return Plan{}, fmt.Errorf("optimizer plan rejected: %w", err)
	}
	return plan, nil
}

func (o *ExternalOptimizer) cancelIdleStopLocked() {
	if o.idleTimer == nil {
		return
	}
	o.idleTimer.Stop()
	o.idleTimer = nil
}

func (o *ExternalOptimizer) scheduleIdleStopLocked() {
	if o.cfg.IdleTimeout <= 0 || o.cmd == nil {
		return
	}
	o.cancelIdleStopLocked()
	var timer *time.Timer
	timer = time.AfterFunc(o.cfg.IdleTimeout, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.idleTimer != timer {
			return
		}
		o.idleTimer = nil
		o.stopLocked()
	})
	o.idleTimer = timer
}

func (o *ExternalOptimizer) buildRequest(slots []Slot, p Params) externalRequest {
	req := externalRequest{
		SchemaVersion: externalOptimizerSchemaVersion,
		RequestID:     uuid.NewString(),
		Settings: externalSettings{
			Mode:                     p.Mode,
			Solver:                   strings.ToUpper(o.cfg.Solver),
			Formulation:              o.cfg.Formulation,
			TimeLimitS:               o.cfg.Timeout.Seconds() * 0.8,
			MIPRelGap:                o.cfg.MIPRelGap,
			ExportOrePerKWh:          p.ExportOrePerKWh,
			ExportBonusOreKwh:        p.ExportBonusOreKwh,
			ExportFeeOreKwh:          p.ExportFeeOreKwh,
			ExportFloorOreKwh:        p.ExportFloorOreKwh,
			MinArbitrageSpreadOreKwh: p.MinArbitrageSpreadOreKwh,
			PVChargeBonusOreKwh:      p.PVChargeBonusOreKwh,
			CVaRWeight:               o.cfg.CVaRWeight,
			CVaRAlpha:                o.cfg.CVaRAlpha,
		},
		Slots:        make([]externalSlot, len(slots)),
		Storages:     []externalStorage{},
		FlexLoads:    []externalFlexLoad{},
		ThermalLoads: []map[string]any{},
	}
	for i, slot := range slots {
		req.Slots[i] = externalSlot{
			StartMs: slot.StartMs, LenMin: slot.LenMin,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre,
			Confidence: slot.Confidence, PVW: slot.PVW, LoadW: slot.LoadW,
			MaxImportW: slot.Limits.MaxImportW, MaxExportW: slot.Limits.MaxExportW,
		}
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
			InitialEnergyWh: p.CapacityWh * p.InitialSoCPct / 100,
			MinEnergyWh:     p.CapacityWh * p.SoCMinPct / 100,
			MaxEnergyWh:     p.CapacityWh * p.SoCMaxPct / 100,
			MaxChargeW:      p.MaxChargeW, MaxDischargeW: p.MaxDischargeW,
			ChargeEfficiency: p.ChargeEfficiency, DischargeEfficiency: p.DischargeEfficiency,
			TerminalPriceOreKWh: p.TerminalSoCPrice,
		}}
	}
	for _, lp := range p.activeLoadpoints() {
		steps := lp.normalizedSteps()
		efficiency := lp.ChargeEfficiency
		if efficiency <= 0 {
			efficiency = 0.9
		}
		minPct, maxPct := lp.MinPct, lp.MaxPct
		if maxPct <= minPct {
			minPct, maxPct = 0, 100
		}
		initialPct := math.Max(minPct, math.Min(maxPct, lp.InitialSoCPct))
		targetPct := math.Max(minPct, math.Min(maxPct, lp.TargetSoCPct))
		req.FlexLoads = append(req.FlexLoads, externalFlexLoad{
			ID: lp.ID, CapacityWh: lp.CapacityWh,
			InitialEnergyWh: lp.CapacityWh * initialPct / 100,
			MaxEnergyWh:     lp.CapacityWh * maxPct / 100,
			TargetEnergyWh:  lp.CapacityWh * targetPct / 100,
			TargetSlot:      lp.TargetSlotIdx, ChargeEfficiency: efficiency,
			MaxChargeW: lp.MaxChargeW, AllowedStepsW: steps,
			SurplusOnly: lp.SurplusOnly, NoStorageToLoad: lp.blocksBatteryToEV(),
		})
	}
	if p.PVUncertaintyW > 0 && p.PVForecastSafetyK > 0 {
		downsidePV := make([]float64, len(slots))
		upsidePV := make([]float64, len(slots))
		loads := make([]float64, len(slots))
		basePV := make([]float64, len(slots))
		hasDaylight := false
		spread := p.PVUncertaintyW * p.PVForecastSafetyK
		for i, slot := range slots {
			loads[i] = slot.LoadW
			basePV[i] = slot.PVW
			if slot.PVW < 0 {
				hasDaylight = true
				generation := -slot.PVW
				downsidePV[i] = -math.Max(0, generation-spread)
				upsidePV[i] = -(generation + spread)
			}
		}
		if hasDaylight {
			req.Scenarios = []map[string]any{
				{"id": "base", "probability": 0.60, "load_w": loads, "pv_w": basePV},
				{"id": "pv-downside", "probability": 0.25, "load_w": loads, "pv_w": downsidePV},
				{"id": "pv-upside", "probability": 0.15, "load_w": loads, "pv_w": upsidePV},
			}
		}
	}
	return req
}

func (r externalResponse) toPlan(slots []Slot, p Params) Plan {
	plan := Plan{
		GeneratedAtMs: time.Now().UnixMilli(), Mode: p.Mode,
		HorizonSlots: len(slots), CapacityWh: p.CapacityWh,
		InitialSoCPct: p.InitialSoCPct, TotalCostOre: r.Plan.TotalCostOre,
		Actions: make([]Action, 0, len(r.Plan.Actions)), Solver: &r.Solver,
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
			SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre,
			PVW: slot.PVW, LoadW: slot.LoadW, Confidence: slot.Confidence,
			BatteryW: candidate.BatteryW, GridW: candidate.GridW,
			SoCPct: candidate.SoCPct, CostOre: candidate.CostOre,
			PVLimitW:        candidate.PVLimitW,
			StoragePowerW:   candidate.StoragePowerW,
			StorageEnergyWh: candidate.StorageEnergy,
		}
		activeLoadpoints := p.activeLoadpoints()
		if len(activeLoadpoints) > 0 {
			action.LoadpointPowerW = make(map[string]float64, len(activeLoadpoints))
			action.LoadpointSoCPctByID = make(map[string]float64, len(activeLoadpoints))
			for lpIdx, lp := range activeLoadpoints {
				powerW := candidate.FlexPowerW[lp.ID]
				socPct := candidate.FlexEnergyWh[lp.ID] / lp.CapacityWh * 100
				action.LoadpointPowerW[lp.ID] = powerW
				action.LoadpointSoCPctByID[lp.ID] = socPct
				if lpIdx == 0 {
					action.LoadpointW = powerW
					action.LoadpointSoCPct = socPct
				}
			}
		}
		action.Reason = reasonFor(slot, action.BatteryW, action.GridW, meanPrice)
		plan.Actions = append(plan.Actions, action)
	}
	return plan
}

func (o *ExternalOptimizer) ensureStartedLocked() error {
	if o.cmd != nil {
		return nil
	}
	cmd := exec.Command(o.cfg.Command[0], o.cfg.Command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if o.cfg.ModuleDir != "" {
		cmd.Env = append(cmd.Env, "PYTHONPATH="+o.cfg.ModuleDir)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("optimizer stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("optimizer stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start optimizer %q: %w", o.cfg.Command[0], err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	o.cmd, o.stdin, o.scanner, o.waitCh = cmd, stdin, bufio.NewScanner(stdout), waitCh
	o.scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return nil
}

func (o *ExternalOptimizer) stopLocked() {
	o.cancelIdleStopLocked()
	if o.cmd == nil {
		return
	}
	_ = o.stdin.Close()
	_ = o.cmd.Process.Kill()
	<-o.waitCh
	o.cmd, o.stdin, o.scanner, o.waitCh = nil, nil, nil, nil
}

func (o *ExternalOptimizer) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancelIdleStopLocked()
	if o.cmd == nil {
		return nil
	}
	_ = o.stdin.Close()
	select {
	case err := <-o.waitCh:
		o.cmd, o.stdin, o.scanner, o.waitCh = nil, nil, nil, nil
		return err
	case <-time.After(time.Second):
		o.stopLocked()
		return nil
	}
}

// ValidatePlan independently replays a candidate plan against the canonical
// site sign convention and current constraints. Solver output is untrusted at
// this boundary: NaN, stale slot alignment, energy drift, illegal EV steps, or
// mode/grid-limit violations reject the entire plan.
func ValidatePlan(slots []Slot, p Params, plan *Plan) error {
	if plan == nil || len(plan.Actions) != len(slots) {
		return fmt.Errorf("action count %d, want %d", len(plan.Actions), len(slots))
	}
	if len(slots) == 0 {
		return errors.New("empty plan")
	}
	soc := p.InitialSoCPct
	storageEnergy := make(map[string]float64, len(p.Storages))
	storageLowerRecovery := make(map[string]float64, len(p.Storages))
	storageUpperRecovery := make(map[string]float64, len(p.Storages))
	for _, storage := range p.Storages {
		storageEnergy[storage.ID] = storage.InitialEnergyWh
		storageLowerRecovery[storage.ID] = math.Max(0, storage.MinEnergyWh-storage.InitialEnergyWh)
		storageUpperRecovery[storage.ID] = math.Max(0, storage.InitialEnergyWh-storage.MaxEnergyWh)
	}
	lowerSoCRecovery := math.Max(0, p.SoCMinPct-p.InitialSoCPct)
	upperSoCRecovery := math.Max(0, p.InitialSoCPct-p.SoCMaxPct)
	activeLoadpoints := p.activeLoadpoints()
	evSoC := make(map[string]float64, len(activeLoadpoints))
	for _, lp := range activeLoadpoints {
		evSoC[lp.ID] = lp.InitialSoCPct
	}
	totalCost := 0.0
	for i, slot := range slots {
		a := plan.Actions[i]
		values := []float64{a.BatteryW, a.GridW, a.SoCPct, a.CostOre, a.LoadpointW, a.LoadpointSoCPct, a.PVLimitW}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("slot %d contains non-finite output", i)
			}
		}
		if a.SlotStartMs != slot.StartMs || a.SlotLenMin != slot.LenMin {
			return fmt.Errorf("slot %d timestamp/length mismatch", i)
		}
		if a.BatteryW > p.MaxChargeW+2 || a.BatteryW < -p.MaxDischargeW-2 {
			return fmt.Errorf("slot %d battery_w %.3f exceeds bounds", i, a.BatteryW)
		}
		dtH := float64(slot.LenMin) / 60
		if len(p.Storages) > 0 {
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
				energyWh := storageEnergy[storage.ID]
				if powerW >= 0 {
					energyWh += powerW * dtH * storage.ChargeEfficiency
				} else {
					energyWh += powerW * dtH / storage.DischargeEfficiency
				}
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
			soc = totalEnergyWh / p.CapacityWh * 100
		} else if a.BatteryW >= 0 {
			soc += a.BatteryW * dtH * p.ChargeEfficiency / p.CapacityWh * 100
		} else {
			soc += a.BatteryW * dtH / p.DischargeEfficiency / p.CapacityWh * 100
		}
		lowerRecovery := math.Max(0, p.SoCMinPct-soc)
		upperRecovery := math.Max(0, soc-p.SoCMaxPct)
		if lowerRecovery > lowerSoCRecovery+0.02 || upperRecovery > upperSoCRecovery+0.02 || math.Abs(a.SoCPct-soc) > 0.02 {
			return fmt.Errorf("slot %d SoC %.4f inconsistent with replay %.4f", i, a.SoCPct, soc)
		}
		lowerSoCRecovery = math.Min(lowerSoCRecovery, lowerRecovery)
		upperSoCRecovery = math.Min(upperSoCRecovery, upperRecovery)
		totalLoadpointW := 0.0
		for lpIdx, lp := range activeLoadpoints {
			powerW := a.LoadpointPowerW[lp.ID]
			reportedSoC := a.LoadpointSoCPctByID[lp.ID]
			if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
				powerW, reportedSoC = a.LoadpointW, a.LoadpointSoCPct
			}
			if math.IsNaN(powerW) || math.IsInf(powerW, 0) || math.IsNaN(reportedSoC) || math.IsInf(reportedSoC, 0) {
				return fmt.Errorf("slot %d loadpoint %s contains non-finite output", i, lp.ID)
			}
			steps := lp.normalizedSteps()
			if !slices.ContainsFunc(steps, func(step float64) bool { return math.Abs(step-powerW) <= 2 }) {
				return fmt.Errorf("slot %d loadpoint %s power %.3f is not an allowed step", i, lp.ID, powerW)
			}
			eff := lp.ChargeEfficiency
			if eff <= 0 {
				eff = 0.9
			}
			evSoC[lp.ID] += powerW * dtH * eff / lp.CapacityWh * 100
			if math.Abs(reportedSoC-evSoC[lp.ID]) > 0.02 {
				return fmt.Errorf("slot %d loadpoint %s SoC %.4f inconsistent with replay %.4f", i, lp.ID, reportedSoC, evSoC[lp.ID])
			}
			totalLoadpointW += powerW
		}
		effectivePVW := slot.PVW
		if a.PVLimitW > 0 {
			if a.PVLimitW > -slot.PVW+2 {
				return fmt.Errorf("slot %d pv_limit_w %.3f exceeds forecast generation %.3f", i, a.PVLimitW, -slot.PVW)
			}
			effectivePVW = -a.PVLimitW
		}
		for lpIdx, lp := range activeLoadpoints {
			powerW := a.LoadpointPowerW[lp.ID]
			if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
				powerW = a.LoadpointW
			}
			if lp.SurplusOnly && powerW > 0 && a.GridW > 50 {
				return fmt.Errorf("slot %d surplus-only loadpoint %s imports from grid", i, lp.ID)
			}
			if lp.SurplusOnly && a.BatteryW > 0 && a.GridW > 50 {
				return fmt.Errorf("slot %d surplus-only loadpoint %s permits grid-funded battery charge", i, lp.ID)
			}
			if powerW > 0 && a.BatteryW < 0 && a.GridW < -50 {
				return fmt.Errorf("slot %d loadpoint %s charges during battery-driven export", i, lp.ID)
			}
			if lp.blocksBatteryToEV() && powerW > 0 && a.BatteryW < 0 {
				houseResidualW := math.Max(0, slot.LoadW+effectivePVW)
				if -a.BatteryW > houseResidualW+50 {
					return fmt.Errorf("slot %d battery discharge feeds loadpoint %s", i, lp.ID)
				}
			}
		}
		wantGridW := slot.LoadW + effectivePVW + a.BatteryW + totalLoadpointW
		if math.Abs(a.GridW-wantGridW) > 2 {
			return fmt.Errorf("slot %d grid balance %.3f, want %.3f", i, a.GridW, wantGridW)
		}
		baseGridW := slot.LoadW + effectivePVW + totalLoadpointW
		if !modeAllows(p.Mode, baseGridW, a.GridW, a.BatteryW) {
			return fmt.Errorf("slot %d violates mode %s: baseline_grid_w=%.9f grid_w=%.9f battery_w=%.9f",
				i, p.Mode, baseGridW, a.GridW, a.BatteryW)
		}
		if !slot.Limits.allowsImport(a.GridW) || !slot.Limits.allowsExport(a.GridW) {
			return fmt.Errorf("slot %d grid_w %.3f violates grid limits", i, a.GridW)
		}
		gridKWh := a.GridW * dtH / 1000
		wantCost := SlotGridCostOre(slot, gridKWh, p)
		if math.Abs(a.CostOre-wantCost) > 0.05 {
			return fmt.Errorf("slot %d cost %.4f, want %.4f", i, a.CostOre, wantCost)
		}
		totalCost += wantCost
	}
	if math.Abs(plan.TotalCostOre-totalCost) > 0.1 {
		return fmt.Errorf("total cost %.4f, want %.4f", plan.TotalCostOre, totalCost)
	}
	return nil
}
