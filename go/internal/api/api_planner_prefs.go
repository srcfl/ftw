package api

import (
	"context"
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

func (s *Server) plannerPrefsSnapshot() (trust config.ForecastTrust, export config.BatteryExport, yamlCustom bool, mappedK float64, mappedMode string) {
	trust, export = s.deps.PlannerPrefs.Get()
	var planner *config.Planner
	if s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		planner = s.deps.Cfg.Planner
		s.deps.CfgMu.RUnlock()
	}
	yamlCustom = planner.YAMLCustomK()
	mappedK = planner.EffectiveSafetyK(trust)
	mappedMode = export.PlannerModeKey()
	return
}

func (s *Server) handleGetPlannerPrefs(w http.ResponseWriter, r *http.Request) {
	trust, export, yamlCustom, mappedK, mappedMode := s.plannerPrefsSnapshot()
	writeJSON(w, 200, map[string]any{
		"forecast_trust": trust,
		"battery_export": export,
		"yaml_custom":    yamlCustom,
		"mapped_k":       mappedK,
		"mapped_mode":    mappedMode,
	})
}

func (s *Server) handleSetPlannerPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ForecastTrust string `json:"forecast_trust"`
		BatteryExport string `json:"battery_export"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	trust, ok := config.ParseForecastTrust(req.ForecastTrust)
	if !ok || req.ForecastTrust == "" {
		writeJSON(w, 400, map[string]string{"error": "forecast_trust must be cautious, balanced, or bold"})
		return
	}
	export, ok := config.ParseBatteryExport(req.BatteryExport)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "battery_export must be unknown, not_allowed, or allowed"})
		return
	}
	if err := s.applyPlannerPrefs(r.Context(), trust, export); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	_, _, yamlCustom, mappedK, mappedMode := s.plannerPrefsSnapshot()
	writeJSON(w, 200, map[string]any{
		"status":         "ok",
		"forecast_trust": trust,
		"battery_export": export,
		"yaml_custom":    yamlCustom,
		"mapped_k":       mappedK,
		"mapped_mode":    mappedMode,
	})
}

func (s *Server) applyPlannerPrefs(ctx context.Context, trust config.ForecastTrust, export config.BatteryExport) error {
	if s.deps.PlannerPrefs == nil {
		s.deps.PlannerPrefs = config.NewPlannerPrefs(trust, export)
	} else {
		s.deps.PlannerPrefs.Set(trust, export)
	}
	if s.deps.State != nil {
		if err := s.deps.State.SaveConfig(config.StateKeyForecastTrust, string(trust)); err != nil {
			return err
		}
		if err := s.deps.State.SaveConfig(config.StateKeyBatteryExport, string(export)); err != nil {
			return err
		}
	}
	mapped := control.Mode(export.PlannerModeKey())
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
			if s.deps.State != nil {
				_ = s.deps.State.SaveConfig("mode", string(mapped))
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
		s.deps.MPC.SetSafetyK(ctx, planner.EffectiveSafetyK(trust))
	}
	return nil
}
