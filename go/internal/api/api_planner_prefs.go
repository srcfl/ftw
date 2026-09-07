package api

import (
	"context"
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// plannerPrefsSnapshot answers with the resolved numeric k and the enum
// derived from it. The enum is never read back from storage here: k is the
// single source of truth, so an old client polling forecast_trust always sees
// the step the slider actually sits nearest.
func (s *Server) plannerPrefsSnapshot() (trust config.ForecastTrust, export config.BatteryExport, safetyK float64, mappedMode string) {
	_, export, storedK := s.deps.PlannerPrefs.Get()
	var planner *config.Planner
	if s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		planner = s.deps.Cfg.Planner
		s.deps.CfgMu.RUnlock()
	}
	safetyK = planner.EffectiveSafetyK(storedK)
	trust = config.TrustFromSafetyK(safetyK)
	mappedMode = export.PlannerModeKey()
	return
}

func (s *Server) handleGetPlannerPrefs(w http.ResponseWriter, r *http.Request) {
	trust, export, safetyK, mappedMode := s.plannerPrefsSnapshot()
	writeJSON(w, 200, map[string]any{
		"forecast_trust": trust,
		"battery_export": export,
		"safety_k":       safetyK,
		"mapped_k":       safetyK,
		"mapped_mode":    mappedMode,
	})
}

func (s *Server) handleSetPlannerPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ForecastTrust string   `json:"forecast_trust"`
		BatteryExport string   `json:"battery_export"`
		SafetyK       *float64 `json:"safety_k"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var safetyK float64
	if req.SafetyK != nil {
		// New client: k wins and forecast_trust is whatever k derives to,
		// so the two can never be posted into disagreement.
		safetyK = config.ClampSafetyK(*req.SafetyK)
	} else {
		trust, ok := config.ParseForecastTrust(req.ForecastTrust)
		if !ok || req.ForecastTrust == "" {
			writeJSON(w, 400, map[string]string{"error": "forecast_trust must be cautious, balanced, or bold, or send safety_k"})
			return
		}
		safetyK = trust.SafetyK()
	}
	export, ok := config.ParseBatteryExport(req.BatteryExport)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "battery_export must be unknown, not_allowed, or allowed"})
		return
	}
	if err := s.applyPlannerPrefs(r.Context(), safetyK, export); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	trust, _, resolvedK, mappedMode := s.plannerPrefsSnapshot()
	writeJSON(w, 200, map[string]any{
		"status":         "ok",
		"forecast_trust": trust,
		"battery_export": export,
		"safety_k":       resolvedK,
		"mapped_k":       resolvedK,
		"mapped_mode":    mappedMode,
	})
}

func (s *Server) applyPlannerPrefs(ctx context.Context, safetyK float64, export config.BatteryExport) error {
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	safetyK = config.ClampSafetyK(safetyK)
	trust := config.TrustFromSafetyK(safetyK)
	mapped := control.Mode(export.PlannerModeKey())
	if s.deps.State != nil {
		var plannerModes []string
		for _, mode := range control.AllModes() {
			if mode.IsPlannerMode() {
				plannerModes = append(plannerModes, string(mode))
			}
		}
		if err := s.deps.State.SavePlannerPreferences(map[string]string{
			config.StateKeySafetyK:       config.FormatSafetyK(safetyK),
			config.StateKeyForecastTrust: string(trust),
			config.StateKeyBatteryExport: string(export),
		}, plannerModes, string(mapped)); err != nil {
			return err
		}
	}
	if s.deps.PlannerPrefs == nil {
		s.deps.PlannerPrefs = config.NewPlannerPrefs(trust, export, safetyK)
	} else {
		s.deps.PlannerPrefs.Set(trust, export, safetyK)
	}
	if s.deps.Ctrl != nil && s.deps.CtrlMu != nil {
		s.deps.CtrlMu.Lock()
		inPlanner := s.deps.Ctrl.Mode.IsPlannerMode()
		s.deps.CtrlMu.Unlock()
		if inPlanner {
			s.deps.CtrlMu.Lock()
			err := s.deps.Ctrl.ApplyMode(mapped)
			s.deps.CtrlMu.Unlock()
			if err != nil {
				return err
			}
			if mm, ok := control.PlannerMPCMode(mapped); ok && s.deps.MPC != nil {
				s.deps.MPC.SetMode(ctx, mm)
			}
		}
	}
	if s.deps.MPC != nil {
		var planner *config.Planner
		if s.deps.Cfg != nil {
			s.deps.CfgMu.RLock()
			planner = s.deps.Cfg.Planner
			s.deps.CfgMu.RUnlock()
		}
		s.deps.MPC.SetSafetyK(ctx, planner.EffectiveSafetyK(safetyK))
	}
	return nil
}
