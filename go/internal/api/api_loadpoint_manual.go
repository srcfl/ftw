package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
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

	// ReleaseAtSoCPct (0–100) turns the hold into "charge now → target,
	// then back to the plan": the controller releases it once the
	// loadpoint's estimated SoC reaches the target. 0 keeps the legacy
	// pin-until-Stop-or-unplug contract.
	ReleaseAtSoCPct float64 `json:"release_at_soc_pct,omitempty"`
}

// manualHoldResponse mirrors the active hold so the operator can
// confirm what's installed. Returned by POST and GET.
type manualHoldResponse struct {
	Active          bool    `json:"active"`
	PowerW          float64 `json:"power_w"`
	PhaseMode       string  `json:"phase_mode,omitempty"`
	PhaseSplitW     float64 `json:"phase_split_w,omitempty"`
	MinPhaseHoldS   int     `json:"min_phase_hold_s,omitempty"`
	Voltage         float64 `json:"voltage,omitempty"`
	MaxAmpsPerPhase float64 `json:"max_amps_per_phase,omitempty"`
	SitePhases      int     `json:"site_phases,omitempty"`
	ExpiresAtMs     int64   `json:"expires_at_ms,omitempty"`
	ReleaseAtSoCPct float64 `json:"release_at_soc_pct,omitempty"`
	// StartedAtMs is when the operator installed the hold; an Update of
	// the amps keeps it. The live account of what the charger did with the
	// hold is `manual` on GET /api/loadpoints.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
}

// maxManualHoldS bounds the hold duration so a forgotten hold can't
// indefinitely override MPC-driven dispatch. 30 minutes is well above
// any realistic diagnostics session. The number itself lives in the
// loadpoint package, shared with the app session's loadpoint.hold op.
const maxManualHoldS = int(loadpoint.MaxManualHold / time.Second)

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
	// hold_s == 0 means a persistent override (operator "Start" / amp
	// slider): no time expiry, released only by DELETE ("Stop") or on
	// unplug. hold_s < 0 is invalid; hold_s > 0 is the bounded
	// diagnostic hold capped at maxManualHoldS.
	if req.HoldS < 0 {
		writeJSON(w, 400, map[string]string{"error": "hold_s must be >= 0"})
		return
	}
	if req.HoldS > maxManualHoldS {
		writeJSON(w, 400, map[string]string{
			"error": "hold_s exceeds maximum (1800)",
		})
		return
	}
	if req.PowerW < 0 || req.PhaseSplitW < 0 || req.Voltage < 0 || req.MaxAmpsPerPhase < 0 || req.MinPhaseHoldS < 0 {
		writeJSON(w, 400, map[string]string{"error": "power and optional phase limits must be >= 0"})
		return
	}
	if req.SitePhases != 0 && req.SitePhases != 1 && req.SitePhases != 3 {
		writeJSON(w, 400, map[string]string{"error": "site_phases must be 1 or 3 when set"})
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
	if req.ReleaseAtSoCPct < 0 || req.ReleaseAtSoCPct > 100 {
		writeJSON(w, 400, map[string]string{
			"error": "release_at_soc_pct must be between 0 and 100",
		})
		return
	}
	// A release target the estimate already meets would install a hold
	// that the controller clears on its next tick — the charger never
	// starts and the caller sees "active" for a few seconds. Refuse it
	// up front and say why, so the caller can raise the target or omit
	// it and charge until the car is full.
	if req.ReleaseAtSoCPct > 0 {
		if st, ok := s.deps.Loadpoints.State(id); ok && st.PluggedIn &&
			st.CurrentSoC*100 >= req.ReleaseAtSoCPct {
			writeJSON(w, 409, map[string]string{
				"error": fmt.Sprintf("car is already at %d %% — raise release_at_soc_pct or omit it to charge until the car is full",
					int(math.Round(st.CurrentSoC*100))),
			})
			return
		}
	}

	// hold_s == 0 → persistent override (no time expiry); hold_s > 0 →
	// bounded diagnostic hold expiring at now+hold_s.
	var expires time.Time
	persistent := req.HoldS == 0
	if !persistent {
		expires = time.Now().Add(time.Duration(req.HoldS) * time.Second)
	}
	hold := loadpoint.ManualHold{
		PowerW:          req.PowerW,
		PhaseMode:       req.PhaseMode,
		PhaseSplitW:     req.PhaseSplitW,
		MinPhaseHoldS:   req.MinPhaseHoldS,
		Voltage:         req.Voltage,
		MaxAmpsPerPhase: req.MaxAmpsPerPhase,
		SitePhases:      req.SitePhases,
		ExpiresAt:       expires,
		Persistent:      persistent,
		ReleaseAtSoC:    req.ReleaseAtSoCPct / 100,
	}
	// An Update of the amps keeps the hold's start, so the manual tab keeps
	// counting from the first press; a fresh hold starts now.
	now := time.Now()
	hold.StartedAt = now
	if prev, ok := s.deps.LoadpointCtrl.GetManualHold(id, now); ok && !prev.StartedAt.IsZero() {
		hold.StartedAt = prev.StartedAt
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

// decorateLoadpointsWithManual fills the manual-override + amp-conversion
// fields on each loadpoint state so the UI can render the manual amp
// slider (range from min/max charge W ÷ phases ÷ voltage) and reflect
// whether a manual hold is currently pinned. Phases come from the
// loadpoint's phase_mode (1p/3p) or the site fuse; voltage from the
// site fuse. Mutates states in place.
func (s *Server) decorateLoadpointsWithManual(states []loadpoint.State) {
	var fusePhases int
	var voltage float64
	if s.deps.CfgMu != nil && s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		fusePhases = s.deps.Cfg.Fuse.Phases
		voltage = s.deps.Cfg.Fuse.Voltage
		s.deps.CfgMu.RUnlock()
	}
	if fusePhases <= 0 {
		fusePhases = 3
	}
	if voltage <= 0 {
		voltage = 230
	}

	phaseModeByID := map[string]string{}
	if s.deps.Loadpoints != nil {
		for _, c := range s.deps.Loadpoints.Configs() {
			phaseModeByID[c.ID] = c.PhaseMode
		}
	}

	now := time.Now()
	chargers := s.chargerReadings()
	for i := range states {
		phases := fusePhases
		switch phaseModeByID[states[i].ID] {
		case "1p":
			phases = 1
		case "3p":
			phases = 3
		}
		states[i].Phases = phases
		states[i].VoltageV = voltage
		reading := chargers[states[i].DriverName]
		status := &loadpoint.ChargerStatus{Known: reading.Known, Available: reading.Known && !reading.Unavailable, Reason: reading.Reason}
		if !reading.UpdatedAt.IsZero() {
			status.UpdatedAtMs = reading.UpdatedAt.UnixMilli()
		}
		if reading.LimitKnown {
			limit := reading.LimitA
			status.LimitA = &limit
		}
		states[i].Charger = status
		if s.deps.LoadpointCtrl != nil {
			h, ok := s.deps.LoadpointCtrl.GetManualHold(states[i].ID, now)
			if ok {
				states[i].ManualActive = true
				states[i].ManualChargeW = h.PowerW
				states[i].ManualReleaseSoC = h.ReleaseAtSoC
			}
			states[i].Manual = loadpoint.ManualStatusFrom(h, ok, states[i], chargers[states[i].DriverName], now)
		}
	}
}

// chargerReadings collects, per EV driver, what the charger last reported
// about the current it allows and why it delivers none. The field names
// follow the EV driver contract (easee_cloud.lua and its siblings): max_a,
// charging, reason_no_current_label, command_stalled. A driver that emits
// none of them still yields a reading, so the manual status knows the
// charger is there but cannot confirm a limit.
func (s *Server) chargerReadings() map[string]loadpoint.ChargerReading {
	out := map[string]loadpoint.ChargerReading{}
	if s.deps.Tel == nil {
		return out
	}
	for _, rd := range s.deps.Tel.ReadingsByType(telemetry.DerEV) {
		var d struct {
			MaxA           *float64 `json:"max_a"`
			Charging       bool     `json:"charging"`
			Reason         string   `json:"reason_no_current_label"`
			CommandStalled bool     `json:"command_stalled"`
			Online         *bool    `json:"is_online"`
		}
		if len(rd.Data) > 0 {
			_ = json.Unmarshal(rd.Data, &d)
		}
		r := loadpoint.ChargerReading{Known: true, UpdatedAt: rd.UpdatedAt, Unavailable: d.Online != nil && !*d.Online, Charging: d.Charging, Reason: d.Reason, Stalled: d.CommandStalled}
		if health := s.deps.Tel.DriverHealth(rd.Driver); health != nil && !health.TelemetryLive() {
			r.Unavailable = true
		}
		if d.MaxA != nil {
			r.LimitA = *d.MaxA
			r.LimitKnown = true
		}
		out[rd.Driver] = r
	}
	return out
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
	resp.ReleaseAtSoCPct = h.ReleaseAtSoC * 100
	if !h.StartedAt.IsZero() {
		resp.StartedAtMs = h.StartedAt.UnixMilli()
	}
	return resp
}
