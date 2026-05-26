package api

import (
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
)

// Manual-hold diagnostics endpoint. Lets an operator pin a loadpoint
// to a fixed dispatch payload (power_w + phase preferences + site
// fuse params) for a bounded duration, bypassing the MPC budget path.
// Used to test driver-level phase decisions on real hardware: hold a
// specific amperage long enough to observe charger behaviour without
// fighting the 5-second control tick. The hold auto-expires; the
// next tick after expiry resumes normal MPC-driven dispatch.
//
// Per the api/CLAUDE.md split convention, this lives in its own file
// and is registered via routes() in api.go.

// manualHoldRequest is the body shape for POST. All fields are
// optional except hold_s. Omitted fields fall through to the
// loadpoint's configured PhaseMode/PhaseSplitW/MinPhaseHoldS and the
// wired site fuse for voltage / max_amps_per_phase / site_phases —
// see Controller.tickOne's hold branch. A minimal `{hold_s: 30,
// power_w: X}` therefore still carries the per-phase fuse clamp
// inputs the driver needs.
type manualHoldRequest struct {
	PowerW          float64 `json:"power_w"`
	PhaseMode       string  `json:"phase_mode,omitempty"`
	PhaseSplitW     float64 `json:"phase_split_w,omitempty"`
	MinPhaseHoldS   int     `json:"min_phase_hold_s,omitempty"`
	Voltage         float64 `json:"voltage,omitempty"`
	MaxAmpsPerPhase float64 `json:"max_amps_per_phase,omitempty"`
	SitePhases      int     `json:"site_phases,omitempty"`
	HoldS           int     `json:"hold_s"`
}

// manualHoldResponse mirrors the active hold so the operator can
// confirm what's installed. Returned by POST and GET.
type manualHoldResponse struct {
	Active          bool    `json:"active"`
	PowerW          float64 `json:"power_w,omitempty"`
	PhaseMode       string  `json:"phase_mode,omitempty"`
	PhaseSplitW     float64 `json:"phase_split_w,omitempty"`
	MinPhaseHoldS   int     `json:"min_phase_hold_s,omitempty"`
	Voltage         float64 `json:"voltage,omitempty"`
	MaxAmpsPerPhase float64 `json:"max_amps_per_phase,omitempty"`
	SitePhases      int     `json:"site_phases,omitempty"`
	ExpiresAtMs     int64   `json:"expires_at_ms,omitempty"`
}

// maxManualHoldS bounds the hold duration so a forgotten hold can't
// indefinitely override MPC-driven dispatch. 30 minutes is well above
// any realistic diagnostics session.
const maxManualHoldS = 30 * 60

// handleLoadpointManualHold installs a manual override on the named
// loadpoint until `now + hold_s`. POST body is manualHoldRequest.
func (s *Server) handleLoadpointManualHold(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not available"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	var req manualHoldRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.HoldS <= 0 {
		writeJSON(w, 400, map[string]string{"error": "hold_s must be > 0"})
		return
	}
	if req.HoldS > maxManualHoldS {
		writeJSON(w, 400, map[string]string{
			"error": "hold_s exceeds maximum (1800)",
		})
		return
	}
	if req.PowerW < 0 {
		writeJSON(w, 400, map[string]string{"error": "power_w must be >= 0"})
		return
	}
	switch req.PhaseMode {
	case "", "auto", "1p", "3p":
	default:
		writeJSON(w, 400, map[string]string{
			"error": "phase_mode must be omitted/empty or one of: auto, 1p, 3p",
		})
		return
	}

	expires := time.Now().Add(time.Duration(req.HoldS) * time.Second)
	hold := loadpoint.ManualHold{
		PowerW:          req.PowerW,
		PhaseMode:       req.PhaseMode,
		PhaseSplitW:     req.PhaseSplitW,
		MinPhaseHoldS:   req.MinPhaseHoldS,
		Voltage:         req.Voltage,
		MaxAmpsPerPhase: req.MaxAmpsPerPhase,
		SitePhases:      req.SitePhases,
		ExpiresAt:       expires,
	}
	s.deps.LoadpointCtrl.SetManualHold(id, hold)
	writeJSON(w, 200, manualHoldResponseFrom(hold, true))
}

// handleLoadpointManualHoldClear cancels any active hold on the
// loadpoint. Idempotent.
func (s *Server) handleLoadpointManualHoldClear(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not available"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	s.deps.LoadpointCtrl.ClearManualHold(id)
	writeJSON(w, 200, manualHoldResponse{Active: false})
}

// handleLoadpointManualHoldGet returns the active hold (if any).
// Useful for the operator UI / scripts that want to verify state
// without re-installing the hold.
func (s *Server) handleLoadpointManualHoldGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not available"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	h, active := s.deps.LoadpointCtrl.GetManualHold(id, time.Now())
	writeJSON(w, 200, manualHoldResponseFrom(h, active))
}

func manualHoldResponseFrom(h loadpoint.ManualHold, active bool) manualHoldResponse {
	resp := manualHoldResponse{Active: active}
	if !active {
		return resp
	}
	resp.PowerW = h.PowerW
	resp.PhaseMode = h.PhaseMode
	resp.PhaseSplitW = h.PhaseSplitW
	resp.MinPhaseHoldS = h.MinPhaseHoldS
	resp.Voltage = h.Voltage
	resp.MaxAmpsPerPhase = h.MaxAmpsPerPhase
	resp.SitePhases = h.SitePhases
	if !h.ExpiresAt.IsZero() {
		resp.ExpiresAtMs = h.ExpiresAt.UnixMilli()
	}
	return resp
}
