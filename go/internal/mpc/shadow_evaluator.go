package mpc

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

const maxShadowObservationGap = time.Minute

// ShadowEvaluation is the accumulated closed-loop score for the active
// champion and the recourse challenger. Both policies see identical realized
// house load, PV, prices, and physical limits, but evolve independent virtual
// storage state. ChallengerMinusChampionOre < 0 means the challenger was
// cheaper over the evaluated interval.
type ShadowEvaluation struct {
	ChampionID                       string             `json:"champion_id"`
	ChallengerID                     string             `json:"challenger_id"`
	StartedAtMs                      int64              `json:"started_at_ms"`
	UpdatedAtMs                      int64              `json:"updated_at_ms"`
	EvaluatedSeconds                 float64            `json:"evaluated_seconds"`
	Samples                          int64              `json:"samples"`
	SkippedSamples                   int64              `json:"skipped_samples"`
	ChampionCostOre                  float64            `json:"champion_cost_ore"`
	ChallengerCostOre                float64            `json:"challenger_cost_ore"`
	ChallengerMinusChampionOre       float64            `json:"challenger_minus_champion_ore"`
	ChampionTerminalValueOre         float64            `json:"champion_terminal_value_ore"`
	ChallengerTerminalValueOre       float64            `json:"challenger_terminal_value_ore"`
	ChampionValuedCostOre            float64            `json:"champion_valued_cost_ore"`
	ChallengerValuedCostOre          float64            `json:"challenger_valued_cost_ore"`
	ChallengerMinusChampionValuedOre float64            `json:"challenger_minus_champion_valued_ore"`
	ChampionVirtualSoCPct            float64            `json:"champion_virtual_soc_pct"`
	ChallengerVirtualSoCPct          float64            `json:"challenger_virtual_soc_pct"`
	ChampionStorageEnergyWh          map[string]float64 `json:"champion_storage_energy_wh,omitempty"`
	ChallengerStorageEnergyWh        map[string]float64 `json:"challenger_storage_energy_wh,omitempty"`
	ChampionClampCount               int64              `json:"champion_clamp_count"`
	ChallengerClampCount             int64              `json:"challenger_clamp_count"`
	Status                           string             `json:"status"`
	LastError                        string             `json:"last_error,omitempty"`
}

type virtualStorage struct {
	id           string
	capacityWh   float64
	minEnergyWh  float64
	maxEnergyWh  float64
	maxChargeW   float64
	maxDischarge float64
	etaCharge    float64
	etaDischarge float64
}

// StatefulShadowEvaluator is intentionally independent of dispatch. It only
// consumes immutable plans plus sampled realized exogenous power and can never
// write a driver command or replace Service.last.
type StatefulShadowEvaluator struct {
	mu sync.Mutex

	champion     *Plan
	challenger   *Plan
	params       Params
	slots        []Slot
	storages     []virtualStorage
	championWh   map[string]float64
	challengerWh map[string]float64
	lastObserved time.Time
	summary      ShadowEvaluation
}

func newStatefulShadowEvaluator() *StatefulShadowEvaluator {
	return &StatefulShadowEvaluator{}
}

func (e *StatefulShadowEvaluator) SetPlans(champion, challenger *Plan, slots []Slot, p Params, now time.Time) {
	if e == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.champion = clonePlan(champion)
	e.challenger = clonePlan(challenger)
	e.params = p
	e.slots = append([]Slot(nil), slots...)

	storages, initial := shadowStorageFleet(p)
	championID := shadowPolicyID(champion, p, storages)
	challengerID := shadowPolicyID(challenger, p, storages)
	policyChanged := challengerID != "" && e.summary.ChallengerID != "" &&
		(championID != e.summary.ChampionID || challengerID != e.summary.ChallengerID)
	if policyChanged {
		e.championWh = cloneEnergyMap(initial)
		e.challengerWh = cloneEnergyMap(initial)
		e.summary = ShadowEvaluation{StartedAtMs: now.UnixMilli(), Status: "warming"}
		e.lastObserved = time.Time{}
	}
	if !sameShadowFleet(e.storages, storages) {
		e.storages = storages
		canResume := e.summary.StartedAtMs > 0 && sameEnergyKeys(e.championWh, initial) && sameEnergyKeys(e.challengerWh, initial)
		if canResume {
			clampEnergyMap(e.championWh, storages)
			clampEnergyMap(e.challengerWh, storages)
		} else {
			e.championWh = cloneEnergyMap(initial)
			e.challengerWh = cloneEnergyMap(initial)
			e.summary = ShadowEvaluation{
				StartedAtMs: now.UnixMilli(), Status: "warming",
			}
		}
		e.lastObserved = time.Time{}
	}
	if e.summary.StartedAtMs == 0 {
		e.summary.StartedAtMs = now.UnixMilli()
	}
	e.summary.ChampionID = championID
	if challengerID != "" {
		e.summary.ChallengerID = challengerID
	}
	if challenger == nil {
		e.summary.Status = "challenger_unavailable"
	} else {
		e.summary.Status = "running"
		e.summary.LastError = ""
	}
	// Never attribute the time spent solving (or the tail of the previous
	// plan) to a newly published action. Scoring resumes with the next sample.
	e.lastObserved = now
	e.updateVirtualSoCLocked()
}

func (e *StatefulShadowEvaluator) SetError(message string, now time.Time) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.challenger = nil
	e.summary.Status = "challenger_unavailable"
	e.summary.LastError = message
	if !now.IsZero() {
		e.summary.UpdatedAtMs = now.UnixMilli()
	}
}

// Restore retains the cumulative score and virtual storage state stored in a
// diagnostic. SetPlans validates the storage topology before continuing it.
func (e *StatefulShadowEvaluator) Restore(summary *ShadowEvaluation) {
	if e == nil || summary == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.summary = cloneShadowEvaluation(*summary)
	e.championWh = cloneEnergyMap(summary.ChampionStorageEnergyWh)
	e.challengerWh = cloneEnergyMap(summary.ChallengerStorageEnergyWh)
	e.lastObserved = time.Time{}
}

// Observe advances both policies over one measured interval. loadW is
// uncontrollable house load (positive); pvW follows site convention (negative
// generation). Long telemetry gaps are skipped instead of extrapolating stale
// samples across an outage.
func (e *StatefulShadowEvaluator) Observe(now time.Time, loadW, pvW float64) ShadowEvaluation {
	if e == nil {
		return ShadowEvaluation{Status: "disabled"}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	if e.lastObserved.IsZero() {
		e.lastObserved = now
		e.summary.UpdatedAtMs = now.UnixMilli()
		return cloneShadowEvaluation(e.summary)
	}
	dt := now.Sub(e.lastObserved)
	e.lastObserved = now
	if dt <= 0 || dt > maxShadowObservationGap || e.champion == nil || e.challenger == nil {
		e.summary.SkippedSamples++
		e.summary.UpdatedAtMs = now.UnixMilli()
		return cloneShadowEvaluation(e.summary)
	}
	championAction, okChampion := shadowActionAt(e.champion, now)
	challengerAction, okChallenger := shadowActionAt(e.challenger, now)
	slot, okSlot := shadowSlotAt(e.slots, now)
	if !okChampion || !okChallenger || !okSlot || !finiteShadowSample(loadW, pvW) {
		e.summary.SkippedSamples++
		e.summary.UpdatedAtMs = now.UnixMilli()
		return cloneShadowEvaluation(e.summary)
	}

	dtH := dt.Hours()
	championCost, championClamped := e.advancePolicyLocked(championAction, slot, e.championWh, loadW, pvW, dtH)
	challengerCost, challengerClamped := e.advancePolicyLocked(challengerAction, slot, e.challengerWh, loadW, pvW, dtH)
	e.summary.ChampionCostOre += championCost
	e.summary.ChallengerCostOre += challengerCost
	e.summary.ChallengerMinusChampionOre = e.summary.ChallengerCostOre - e.summary.ChampionCostOre
	e.summary.EvaluatedSeconds += dt.Seconds()
	e.summary.Samples++
	if championClamped {
		e.summary.ChampionClampCount++
	}
	if challengerClamped {
		e.summary.ChallengerClampCount++
	}
	e.summary.UpdatedAtMs = now.UnixMilli()
	e.summary.Status = "running"
	e.updateVirtualSoCLocked()
	return cloneShadowEvaluation(e.summary)
}

func (e *StatefulShadowEvaluator) Snapshot() *ShadowEvaluation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := cloneShadowEvaluation(e.summary)
	return &snapshot
}

func (e *StatefulShadowEvaluator) advancePolicyLocked(action Action, slot Slot, energy map[string]float64, loadW, pvW, dtH float64) (float64, bool) {
	effectivePV := pvW
	if action.PVLimitW > 0 && -effectivePV > action.PVLimitW {
		effectivePV = -action.PVLimitW
	}
	netW := math.Max(0, loadW) + math.Min(0, effectivePV)
	desired := make(map[string]float64, len(e.storages))
	if len(action.StoragePowerW) > 0 {
		for _, spec := range e.storages {
			desired[spec.id] = action.StoragePowerW[spec.id]
		}
	} else if len(e.storages) > 0 {
		desired[e.storages[0].id] = action.BatteryW
	}

	clamped := false
	totalPower := 0.0
	for _, spec := range e.storages {
		power := desired[spec.id]
		bounded := clampShadowStoragePower(power, energy[spec.id], spec, dtH)
		if math.Abs(bounded-power) > 0.1 {
			clamped = true
		}
		desired[spec.id] = bounded
		totalPower += bounded
	}

	allowedPower := totalPower
	switch e.params.Mode {
	case ModeSelfConsumption:
		if allowedPower > 0 {
			allowedPower = math.Min(allowedPower, math.Max(0, -netW))
		} else {
			allowedPower = math.Max(allowedPower, -math.Max(0, netW))
		}
	case ModeCheapCharge, ModePassiveArbitrage:
		if allowedPower < 0 {
			allowedPower = math.Max(allowedPower, -math.Max(0, netW))
		}
	}
	maxImport := slot.Limits.MaxImportW
	maxExport := slot.Limits.MaxExportW
	if maxImport > 0 {
		allowedPower = math.Min(allowedPower, maxImport-netW)
	}
	if maxExport > 0 {
		allowedPower = math.Max(allowedPower, -maxExport-netW)
	}
	// A safety projection may reduce a requested action to zero but must never
	// invent the opposite action merely because uncontrollable site load already
	// violates a grid bound.
	if totalPower > 0 {
		allowedPower = math.Max(0, allowedPower)
	} else if totalPower < 0 {
		allowedPower = math.Min(0, allowedPower)
	} else {
		allowedPower = 0
	}
	if math.Abs(allowedPower-totalPower) > 0.1 {
		clamped = true
		if math.Abs(totalPower) > 1e-9 {
			scale := allowedPower / totalPower
			for id, power := range desired {
				desired[id] = power * scale
			}
		}
		totalPower = allowedPower
	}

	for _, spec := range e.storages {
		power := desired[spec.id]
		if power >= 0 {
			energy[spec.id] += power * dtH * spec.etaCharge
		} else {
			energy[spec.id] += power * dtH / spec.etaDischarge
		}
		// Operating-band violations present at initialization recover gradually,
		// matching the optimizer and Go validator. Only physical empty/full
		// bounds are unconditional here; clampShadowStoragePower prevents an
		// existing operating-band violation from worsening.
		energy[spec.id] = math.Max(0, math.Min(spec.capacityWh, energy[spec.id]))
	}
	gridW := netW + totalPower
	return SlotGridCostOre(slot, gridW*dtH/1000.0, e.params), clamped
}

func shadowStorageFleet(p Params) ([]virtualStorage, map[string]float64) {
	if len(p.Storages) == 0 {
		etaC, etaD := p.ChargeEfficiency, p.DischargeEfficiency
		if etaC <= 0 {
			etaC = 0.95
		}
		if etaD <= 0 {
			etaD = 0.95
		}
		return []virtualStorage{{
			id: "home-battery", capacityWh: p.CapacityWh,
			minEnergyWh: p.CapacityWh * p.SoCMinPct / 100,
			maxEnergyWh: p.CapacityWh * p.SoCMaxPct / 100,
			maxChargeW:  p.MaxChargeW, maxDischarge: p.MaxDischargeW,
			etaCharge: etaC, etaDischarge: etaD,
		}}, map[string]float64{"home-battery": p.CapacityWh * p.InitialSoCPct / 100}
	}
	storages := make([]virtualStorage, 0, len(p.Storages))
	initial := make(map[string]float64, len(p.Storages))
	for _, s := range p.Storages {
		etaC, etaD := s.ChargeEfficiency, s.DischargeEfficiency
		if etaC <= 0 {
			etaC = 0.95
		}
		if etaD <= 0 {
			etaD = 0.95
		}
		storages = append(storages, virtualStorage{
			id: s.ID, capacityWh: s.CapacityWh, minEnergyWh: s.MinEnergyWh,
			maxEnergyWh: s.MaxEnergyWh, maxChargeW: s.MaxChargeW,
			maxDischarge: s.MaxDischargeW, etaCharge: etaC,
			etaDischarge: etaD,
		})
		initial[s.ID] = s.InitialEnergyWh
	}
	sort.Slice(storages, func(i, j int) bool { return storages[i].id < storages[j].id })
	return storages, initial
}

func clampShadowStoragePower(power, energyWh float64, spec virtualStorage, dtH float64) float64 {
	if dtH <= 0 {
		return 0
	}
	if power >= 0 {
		limit := math.Min(spec.maxChargeW, math.Max(0, spec.maxEnergyWh-energyWh)/(dtH*spec.etaCharge))
		return math.Min(power, limit)
	}
	limit := math.Min(spec.maxDischarge, math.Max(0, energyWh-spec.minEnergyWh)*spec.etaDischarge/dtH)
	return math.Max(power, -limit)
}

func shadowActionAt(plan *Plan, now time.Time) (Action, bool) {
	if plan == nil {
		return Action{}, false
	}
	nowMs := now.UnixMilli()
	for i, action := range plan.Actions {
		endMs := action.SlotStartMs + int64(action.SlotLenMin)*60*1000
		if nowMs >= action.SlotStartMs && nowMs < endMs {
			if plan.Solver != nil && plan.Solver.ScenarioPolicy == "recourse" &&
				plan.Solver.NonAnticipativeSlots > 0 && i >= plan.Solver.NonAnticipativeSlots {
				return Action{}, false
			}
			return action, true
		}
	}
	return Action{}, false
}

func shadowSlotAt(slots []Slot, now time.Time) (Slot, bool) {
	nowMs := now.UnixMilli()
	for _, slot := range slots {
		endMs := slot.StartMs + int64(slot.LenMin)*60*1000
		if nowMs >= slot.StartMs && nowMs < endMs {
			return slot, true
		}
	}
	return Slot{}, false
}

func (e *StatefulShadowEvaluator) updateVirtualSoCLocked() {
	capacity := 0.0
	for _, s := range e.storages {
		capacity += s.capacityWh
	}
	if capacity <= 0 {
		return
	}
	e.summary.ChampionStorageEnergyWh = cloneEnergyMap(e.championWh)
	e.summary.ChallengerStorageEnergyWh = cloneEnergyMap(e.challengerWh)
	e.summary.ChampionVirtualSoCPct = sumEnergy(e.championWh) / capacity * 100
	e.summary.ChallengerVirtualSoCPct = sumEnergy(e.challengerWh) / capacity * 100
	e.summary.ChampionTerminalValueOre = e.params.TerminalSoCPrice * sumEnergy(e.championWh) / 1000
	e.summary.ChallengerTerminalValueOre = e.params.TerminalSoCPrice * sumEnergy(e.challengerWh) / 1000
	e.summary.ChampionValuedCostOre = e.summary.ChampionCostOre - e.summary.ChampionTerminalValueOre
	e.summary.ChallengerValuedCostOre = e.summary.ChallengerCostOre - e.summary.ChallengerTerminalValueOre
	e.summary.ChallengerMinusChampionValuedOre = e.summary.ChallengerValuedCostOre - e.summary.ChampionValuedCostOre
}

func sameShadowFleet(a, b []virtualStorage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || math.Abs(a[i].capacityWh-b[i].capacityWh) > 0.1 ||
			math.Abs(a[i].minEnergyWh-b[i].minEnergyWh) > 0.1 || math.Abs(a[i].maxEnergyWh-b[i].maxEnergyWh) > 0.1 {
			return false
		}
	}
	return true
}

func clonePlan(plan *Plan) *Plan {
	if plan == nil {
		return nil
	}
	out := *plan
	out.Actions = append([]Action(nil), plan.Actions...)
	return &out
}

func shadowPolicyID(plan *Plan, p Params, storages []virtualStorage) string {
	if plan == nil || plan.Solver == nil {
		return ""
	}
	policy := plan.Solver.ScenarioPolicy
	if policy == "" {
		policy = "shared"
	}
	version := plan.Solver.PolicyVersion
	if version == "" {
		version = policy
	}
	id := fmt.Sprintf("%s:%s:%s:%s:cvar=%g@%g", plan.Solver.Engine, plan.Solver.Backend,
		version, plan.Mode, plan.Solver.CVaRWeight, plan.Solver.CVaRAlpha)
	id += fmt.Sprintf(":terminal=%g:export=%g/%g/%g", p.TerminalSoCPrice,
		p.ExportOrePerKWh, p.ExportBonusOreKwh, p.ExportFeeOreKwh)
	id += fmt.Sprintf(":spread=%g:pvbonus=%g:pvsafety=%g", p.MinArbitrageSpreadOreKwh,
		p.PVChargeBonusOreKwh, p.PVForecastSafetyK)
	if p.ExportFloorOreKwh != nil {
		id += fmt.Sprintf(":exportfloor=%g", *p.ExportFloorOreKwh)
	}
	for _, storage := range storages {
		id += fmt.Sprintf(":%s=%g/%g/%g/%g/%g/%g/%g", storage.id, storage.capacityWh,
			storage.minEnergyWh, storage.maxEnergyWh, storage.maxChargeW,
			storage.maxDischarge, storage.etaCharge, storage.etaDischarge)
	}
	return id
}

func cloneEnergyMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sameEnergyKeys(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			return false
		}
	}
	return true
}

func clampEnergyMap(energy map[string]float64, storages []virtualStorage) {
	for _, storage := range storages {
		energy[storage.id] = math.Max(0, math.Min(storage.capacityWh, energy[storage.id]))
	}
}

func cloneShadowEvaluation(in ShadowEvaluation) ShadowEvaluation {
	in.ChampionStorageEnergyWh = cloneEnergyMap(in.ChampionStorageEnergyWh)
	in.ChallengerStorageEnergyWh = cloneEnergyMap(in.ChallengerStorageEnergyWh)
	return in
}

func sumEnergy(values map[string]float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total
}

func finiteShadowSample(loadW, pvW float64) bool {
	return !math.IsNaN(loadW) && !math.IsInf(loadW, 0) && loadW >= 0 &&
		!math.IsNaN(pvW) && !math.IsInf(pvW, 0) && pvW <= 0
}
