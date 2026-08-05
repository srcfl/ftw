package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Battery manual-hold endpoint. Pins the pool or one named battery to
// a fixed power for a bounded duration. A scoped request binds the
// runtime driver name to its current hardware identity. Core SoC,
// power, slew, reserve, and fuse limits still apply.
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
	Driver    string  `json:"driver,omitempty"`
}

// batteryManualHoldResponse mirrors the active hold so the operator
// can confirm what's installed.
type batteryManualHoldResponse struct {
	Active      bool    `json:"active"`
	Direction   string  `json:"direction,omitempty"`
	PowerW      float64 `json:"power_w,omitempty"`
	Driver      string  `json:"driver,omitempty"`
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
	deviceID := ""
	if req.Driver != "" {
		var status int
		deviceID, status, err = s.resolveBatteryManualHoldTarget(req.Driver)
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
	}

	expires := time.Now().Add(time.Duration(req.HoldS) * time.Second)
	hold := control.BatteryManualHold{
		Driver:    req.Driver,
		DeviceID:  deviceID,
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
		"driver", req.Driver,
	)
	writeJSON(w, 200, batteryManualHoldResponseFrom(hold, true))
}

func (s *Server) resolveBatteryManualHoldTarget(driver string) (string, int, error) {
	if s.deps.CapMu == nil || s.deps.Capacities == nil {
		return "", http.StatusServiceUnavailable, apiError("battery control inventory not available")
	}
	s.deps.CapMu.RLock()
	_, known := s.deps.Capacities[driver]
	s.deps.CapMu.RUnlock()
	if !known {
		return "", http.StatusBadRequest, apiError("unknown controllable battery driver")
	}
	if s.deps.Tel == nil || s.deps.BatteryIdentity == nil {
		return "", http.StatusServiceUnavailable, apiError("battery control state not available")
	}
	health := s.deps.Tel.DriverHealth(driver)
	reading := s.deps.Tel.Get(driver, telemetry.DerBattery)
	if health == nil || !health.IsOnline() || reading == nil || reading.SoC == nil {
		return "", http.StatusConflict, apiError("battery driver is not ready for a scoped hold")
	}
	deviceID, ok := s.deps.BatteryIdentity(driver)
	if !ok || deviceID == "" {
		return "", http.StatusConflict, apiError("battery hardware identity is not available")
	}
	return deviceID, http.StatusOK, nil
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
	resp.Driver = h.Driver
	if !h.ExpiresAt.IsZero() {
		resp.ExpiresAtMs = h.ExpiresAt.UnixMilli()
	}
	return resp
}
