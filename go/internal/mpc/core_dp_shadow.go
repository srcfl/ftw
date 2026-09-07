package mpc

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type coreDPShadowRequest struct {
	champion   Plan
	slots      []Slot
	params     Params
	reason     string
	replanAtMs int64
}

// startCoreDPShadow runs at most one bounded comparison, after publication.
// Results belong to a decision ID and can never replace the active actions.
func (s *Service) startCoreDPShadow(champion Plan, slots []Slot, p Params, reason string, replanAtMs int64) {
	if coreDPModelError(p) != nil {
		return
	}
	s.mu.Lock()
	if s.stopping || s.last == nil || s.last.DecisionID != champion.DecisionID {
		s.mu.Unlock()
		return
	}
	if s.shadowBusy {
		s.pendingCoreShadow = &coreDPShadowRequest{champion, slots, p, reason, replanAtMs}
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.shadowBusy, s.shadowCancel = true, cancel
	s.shadowWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.shadowWG.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mpc: Core DP shadow panicked", "panic", r, "decision_id", champion.DecisionID)
			}
			s.finishCoreDPShadow()
		}()
		start := time.Now()
		shadow, err := OptimizeContext(ctx, slots, p)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err == nil {
			err = ValidatePlan(slots, p, &shadow)
		}
		block := &ShadowPlan{ForecastBasis: "same downside input, Core DP shadow", Solver: coreSolverInfo(p, msSince(start))}
		if err != nil {
			block.Solver.Status = "rejected"
			block.Solver.FallbackReason = err.Error()
			slog.Warn("mpc: Core DP shadow rejected; active plan unchanged", "decision_id", champion.DecisionID, "err", err)
		} else {
			shadow.Solver = block.Solver
			basis := block.ForecastBasis
			block = compareDPShadow(champion, shadow)
			block.ForecastBasis, block.Solver = basis, shadow.Solver
			// Both totals use Core's grid cost model on validated actions.
			activeOre := replayedGridCost(slots, p, champion)
			shadowOre := replayedGridCost(slots, p, shadow)
			block.TotalCostOre = shadowOre
			block.ActiveMinusShadowOre = activeOre - shadowOre
			block.ActiveTerminalCorrectedOre = terminalCorrectedOre(activeOre, planEndSoC(&champion), p)
			block.TerminalCorrectedOre = terminalCorrectedOre(shadowOre, planEndSoC(&shadow), p)
			block.ActiveMinusShadowTerminalCorrectedOre = block.ActiveTerminalCorrectedOre - block.TerminalCorrectedOre
			if block.FirstAction != nil {
				block.FirstAction.EMSMode, _, _ = actionToSlot(*block.FirstAction, p.Mode)
			}
			slog.Info("mpc: Energyplan primary vs Core DP shadow", "decision_id", champion.DecisionID,
				"energyplan_minus_core_ore_terminal_corrected", block.ActiveMinusShadowTerminalCorrectedOre,
				"core_solve_ms", block.Solver.SolveMs)
		}
		s.recordCoreDPShadow(champion, slots, p, reason, replanAtMs, block)
	}()
}

func (s *Service) finishCoreDPShadow() {
	s.mu.Lock()
	s.shadowBusy, s.shadowCancel = false, nil
	pending := s.pendingCoreShadow
	s.pendingCoreShadow = nil
	s.mu.Unlock()
	if pending != nil {
		s.startCoreDPShadow(pending.champion, pending.slots, pending.params, pending.reason, pending.replanAtMs)
	}
}

func replayedGridCost(slots []Slot, p Params, plan Plan) float64 {
	total := 0.0
	for i, slot := range slots {
		total += SlotGridCostOre(slot, plan.Actions[i].GridW*float64(slot.LenMin)/60000, p)
	}
	return total
}

func (s *Service) recordCoreDPShadow(champion Plan, slots []Slot, p Params, reason string, replanAtMs int64, block *ShadowPlan) {
	s.mu.Lock()
	current := !s.stopping && s.last != nil && s.last.DecisionID == champion.DecisionID
	if current {
		updated := *s.last
		updated.DPShadow = block
		// Preserve existing permission when adding comparison data. A late
		// shadow with the same decision ID cannot activate a restored archive.
		if s.executionPlan == s.last {
			s.executionPlan = &updated
		}
		s.last = &updated
	}
	saveDiag, zone := s.SaveDiag, s.Zone
	s.mu.Unlock()
	if !current || saveDiag == nil {
		return
	}
	champion.DPShadow = block
	if d := buildDiagnostic(&champion, slots, p, zone, replanAtMs, reason); d != nil {
		if err := saveDiag(d, reason); err != nil {
			slog.Warn("mpc: persist Core DP shadow failed", "decision_id", champion.DecisionID, "err", err)
		}
	}
}
