package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/control"
)

// Battery manual-hold endpoint. Pins the aggregate battery setpoint to
// a fixed power (charge / discharge / idle) for a bounded duration,
// bypassing both the active control mode and the MPC. SoC clamps,
// per-driver capability caps, slew, and the site fuse guard still
// apply on the resulting target — operators cannot override safety.
//
// Sibling of api_loadpoint_manual.go and registered alongside it in
// api.go's routes() table.

// batteryManualHoldRequest is the body shape for POST. The wire format
// uses a `direction` enum + non-negative `power_w` so the client never
// has to think about site sign convention; the handler converts to a
// signed PowerW (charge=+, discharge=−, idle=0) for control.State.
type batteryManualHoldRequest struct {
	Direction string  `json:"direction"`
	PowerW    float64 `json:"power_w"`
	HoldS     int     `json:"hold_s"`
}

// batteryManualHoldResponse mirrors the active hold so the operator
// can confirm what's installed.
type batteryManualHoldResponse struct {
	Active      bool    `json:"active"`
	Direction   string  `json:"direction,omitempty"`
	PowerW      float64 `json:"power_w,omitempty"`
	ExpiresAtMs int64   `json:"expires_at_ms,omitempty"`
}

// maxBatteryManualHoldS bounds the hold duration. 30 minutes matches
// the loadpoint hold cap so a forgotten override can't indefinitely
// block planner-driven dispatch.
const maxBatteryManualHoldS = 30 * 60

func (s *Server) handleBatteryManualHold(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	var req batteryManualHoldRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.HoldS <= 0 {
		writeJSON(w, 400, map[string]string{"error": "hold_s must be > 0"})
		return
	}
	if req.HoldS > maxBatteryManualHoldS {
		writeJSON(w, 400, map[string]string{
			"error": "hold_s exceeds maximum (1800)",
		})
		return
	}
	if req.PowerW < 0 {
		writeJSON(w, 400, map[string]string{"error": "power_w must be >= 0"})
		return
	}
	signed, err := signedPowerForDirection(req.Direction, req.PowerW)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	expires := time.Now().Add(time.Duration(req.HoldS) * time.Second)
	hold := control.BatteryManualHold{
		PowerW:    signed,
		ExpiresAt: expires,
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetBatteryManualHold(hold)
	s.deps.CtrlMu.Unlock()

	slog.Info("battery manual hold installed",
		"direction", req.Direction,
		"power_w", req.PowerW,
		"signed_w", signed,
		"hold_s", req.HoldS,
	)
	writeJSON(w, 200, batteryManualHoldResponseFrom(hold, true))
}

func (s *Server) handleBatteryManualHoldClear(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.ClearBatteryManualHold()
	s.deps.CtrlMu.Unlock()
	slog.Info("battery manual hold cleared")
	writeJSON(w, 200, batteryManualHoldResponse{Active: false})
}

func (s *Server) handleBatteryManualHoldGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	s.deps.CtrlMu.Lock()
	h, active := s.deps.Ctrl.GetBatteryManualHold(time.Now())
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, batteryManualHoldResponseFrom(h, active))
}

// signedPowerForDirection converts the wire format (direction + magnitude)
// to site-signed power. Direction "idle" forces magnitude to 0 so the
// caller doesn't need to send `power_w: 0` explicitly.
func signedPowerForDirection(direction string, magnitude float64) (float64, error) {
	switch direction {
	case "charge":
		return magnitude, nil
	case "discharge":
		return -magnitude, nil
	case "idle":
		return 0, nil
	default:
		return 0, errInvalidDirection
	}
}

type apiError string

func (e apiError) Error() string { return string(e) }

const errInvalidDirection = apiError("direction must be one of: charge, discharge, idle")

func batteryManualHoldResponseFrom(h control.BatteryManualHold, active bool) batteryManualHoldResponse {
	resp := batteryManualHoldResponse{Active: active}
	if !active {
		return resp
	}
	switch {
	case h.PowerW > 0:
		resp.Direction = "charge"
		resp.PowerW = h.PowerW
	case h.PowerW < 0:
		resp.Direction = "discharge"
		resp.PowerW = -h.PowerW
	default:
		resp.Direction = "idle"
		resp.PowerW = 0
	}
	if !h.ExpiresAt.IsZero() {
		resp.ExpiresAtMs = h.ExpiresAt.UnixMilli()
	}
	return resp
}
