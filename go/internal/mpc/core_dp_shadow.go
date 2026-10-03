package mpc

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// A shadow that runs past its deadline compares nothing; on a Raspberry Pi 4
// that is most shadows with a car plugged in. After a timeout, Core skips
// shadows at least that large for an hour, then tries one again: the horizon
// shrinks through the day, so a later one may fit.
const (
	coreDPShadowTimeout = 10 * time.Second
	coreDPShadowRetry   = time.Hour
	coreDPShadowBasis   = "same downside input, Core DP shadow"
)

type coreDPShadowRequest struct {
	champion   Plan
	slots      []Slot
	params     Params
	reason     string
	replanAtMs int64
}

// coreDPShadowSkip remembers the last shadow that ran out of time: its DP work
// per slot, and when Core may try a shadow that large again.
type coreDPShadowSkip struct {
	work  int64
	until time.Time
}

// coreDPSlotWork counts the choices Core DP weighs in each slot: battery SoC
// levels times battery power levels, times EV SoC levels and charger steps
// when a car is plugged in. Solve time grows with it and with the slot count.
func coreDPSlotWork(p Params) int64 {
	work := int64(max(p.SoCLevels, 3)) * int64(max(p.ActionLevels, 3))
	if lp := p.Loadpoint; lp.active() {
		work *= int64(lp.Levels) * int64(len(lp.normalizedSteps()))
	}
	return work
}

// startCoreDPShadow runs at most one bounded comparison, after publication.
// Results belong to a decision ID and can never replace the active actions.
// A skipped shadow is recorded with its reason, so it never looks missing.
func (s *Service) startCoreDPShadow(champion Plan, slots []Slot, p Params, reason string, replanAtMs int64) {
	if coreDPModelError(p) != nil {
		return
	}
	work := coreDPSlotWork(p)
	s.mu.Lock()
	if s.stopping || s.last == nil || s.last.DecisionID != champion.DecisionID {
		s.mu.Unlock()
		return
	}
	// A clock stepped back after boot must not stretch the skip past an hour.
	if skip, now := s.shadowSkip, s.planningNow(); work >= skip.work && now.Before(skip.until) && skip.until.Sub(now) <= coreDPShadowRetry {
		s.mu.Unlock()
		block := &ShadowPlan{ForecastBasis: coreDPShadowBasis, Solver: coreSolverInfo(p, 0)}
		block.Solver.Status = "skipped"
		block.Solver.FallbackReason = "skipped: a shadow no larger than this one ran out of time; next try after " +
			skip.until.UTC().Format(time.RFC3339)
		slog.Info("mpc: Core DP shadow skipped", "decision_id", champion.DecisionID, "reason", block.Solver.FallbackReason)
		s.recordCoreDPShadow(champion, slots, p, reason, replanAtMs, block)
		return
	}
	if s.shadowBusy {
		s.pendingCoreShadow = &coreDPShadowRequest{champion, slots, p, reason, replanAtMs}
		s.mu.Unlock()
		return
	}
	timeout := s.shadowTimeout
	if timeout == 0 {
		timeout = coreDPShadowTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		if errors.Is(err, context.DeadlineExceeded) {
			until := s.planningNow().Add(coreDPShadowRetry)
			s.mu.Lock()
			s.shadowSkip = coreDPShadowSkip{work: work, until: until}
			s.mu.Unlock()
			slog.Warn("mpc: Core DP shadow ran out of time; skipping shadows this large",
				"decision_id", champion.DecisionID, "dp_work_per_slot", work, "next_try", until)
		}
		if err == nil {
			err = ValidatePlan(slots, p, &shadow)
		}
		block := &ShadowPlan{ForecastBasis: coreDPShadowBasis, Solver: coreSolverInfo(p, msSince(start))}
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
		total += SlotGridCostOre(slot, plan.Actions[i].GridW*slot.DurationHours()/1000, p)
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
