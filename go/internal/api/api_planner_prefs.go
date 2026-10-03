package api

import (
	"errors"
	"net/http"

	"github.com/srcfl/ftw/go/internal/appproto"
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
	// Each control sends only what it changes; the box keeps the rest.
	var safetyK *float64
	if req.SafetyK != nil {
		// New client: k wins and forecast_trust is whatever k derives to,
		// so the two can never be posted into disagreement.
		k := config.ClampSafetyK(*req.SafetyK)
		safetyK = &k
	} else if req.ForecastTrust != "" {
		trust, ok := config.ParseForecastTrust(req.ForecastTrust)
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "forecast_trust must be cautious, balanced, or bold, or send safety_k"})
			return
		}
		k := trust.SafetyK()
		safetyK = &k
	}
	var export *config.BatteryExport
	if req.BatteryExport != "" {
		e, ok := config.ParseBatteryExport(req.BatteryExport)
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "battery_export must be unknown, not_allowed, or allowed"})
			return
		}
		export = &e
	}
	if safetyK == nil && export == nil {
		writeJSON(w, 400, map[string]string{"error": "send safety_k, forecast_trust or battery_export"})
		return
	}
	saved, err := s.applyPlannerChange(safetyK, export)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	trust, _, resolvedK, mappedMode := s.plannerPrefsSnapshot()
	writeJSON(w, 200, map[string]any{
		"status":         "ok",
		"forecast_trust": trust,
		"battery_export": saved,
		"safety_k":       resolvedK,
		"mapped_k":       resolvedK,
		"mapped_mode":    mappedMode,
	})
}

// applyPlannerChange writes what a client sent and keeps the other
// preference as the box holds it when the write runs. It reads under the same
// lock as every preference write, so two clients changing different
// preferences never undo each other.
func (s *Server) applyPlannerChange(safetyK *float64, export *config.BatteryExport) (config.BatteryExport, error) {
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	_, e, k, _ := s.plannerPrefsSnapshot()
	if safetyK != nil {
		k = *safetyK
	}
	if export != nil {
		e = *export
	}
	return e, s.applyPlannerPrefsLocked(k, e)
}

func (s *Server) applyPlannerPrefs(safetyK float64, export config.BatteryExport) error {
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	return s.applyPlannerPrefsLocked(safetyK, export)
}

func (s *Server) applyPlannerPrefsLocked(safetyK float64, export config.BatteryExport) error {
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
				s.deps.MPC.SetMode(mm)
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
		s.deps.MPC.SetSafetyK(planner.EffectiveSafetyK(safetyK))
	}
	return nil
}

// ApplyPlannerPrefs is the session door into the same write POST
// /api/planner/prefs performs. The mapped mode in the snapshot is this
// server's answer; the caller does not choose it.
func (s *Server) ApplyPlannerPrefs(safetyK float64, export string) (appproto.PlannerPrefsSnapshot, error) {
	exp, ok := config.ParseBatteryExport(export)
	if !ok {
		return appproto.PlannerPrefsSnapshot{}, errors.New("battery_export must be unknown, not_allowed, or allowed")
	}
	if err := s.applyPlannerPrefs(safetyK, exp); err != nil {
		return appproto.PlannerPrefsSnapshot{}, err
	}
	_, got, k, mapped := s.plannerPrefsSnapshot()
	return appproto.PlannerPrefsSnapshot{
		SafetyK:    k,
		Export:     string(got),
		MappedMode: mapped,
	}, nil
}
