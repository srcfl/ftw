package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// PV manual-hold endpoint. Pins a PV curtail cap for a bounded
// duration, overriding whatever the planner's slot directive says
// about PVLimitW. Primary use case: operator-side verification that
// the curtail action actually reaches the inverter, without waiting
// for the MPC to organically trigger a negative-price slot.
//
// Body fields:
//   - driver:    optional. "" (or missing) = site-aggregate hold,
//                splits LimitW across SupportsPVCurtail drivers
//                proportionally to live |PV|. Non-empty = scope hold
//                to that one driver only.
//   - limit_w:   optional. Absolute power cap, ≥ 0. Mutually
//                exclusive with limit_pct.
//   - limit_pct: optional. Percent (0–100) of the driver's configured
//                nominal_w (the inverter's rated AC output). Driver-
//                scoped uses that one driver's nominal; site-aggregate
//                uses the sum across SupportsPVCurtail drivers. Falls
//                back to live |PV| only when nominal_w isn't set on
//                any matching driver. Mutually exclusive with limit_w.
//   - hold_s:    required, 1..1800.
//
// Sibling of api_battery_manual.go.

type pvManualHoldRequest struct {
	Driver   string   `json:"driver,omitempty"`
	LimitW   *float64 `json:"limit_w,omitempty"`
	LimitPct *float64 `json:"limit_pct,omitempty"`
	HoldS    int      `json:"hold_s"`
}

type pvManualHoldResponse struct {
	Active      bool    `json:"active"`
	Driver      string  `json:"driver,omitempty"`
	LimitW      float64 `json:"limit_w,omitempty"`
	ExpiresAtMs int64   `json:"expires_at_ms,omitempty"`
}

const maxPVManualHoldS = 30 * 60

func (s *Server) handlePVManualHold(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	var req pvManualHoldRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.HoldS <= 0 {
		writeJSON(w, 400, map[string]string{"error": "hold_s must be > 0"})
		return
	}
	if req.HoldS > maxPVManualHoldS {
		writeJSON(w, 400, map[string]string{"error": "hold_s exceeds maximum (1800)"})
		return
	}
	if (req.LimitW == nil) == (req.LimitPct == nil) {
		writeJSON(w, 400, map[string]string{"error": "exactly one of limit_w or limit_pct required"})
		return
	}

	// Snapshot what we need from State + Tel under their respective locks.
	s.deps.CtrlMu.Lock()
	supports := map[string]bool{}
	for d, v := range s.deps.Ctrl.SupportsPVCurtail {
		if v {
			supports[d] = true
		}
	}
	s.deps.CtrlMu.Unlock()

	if req.Driver != "" && !supports[req.Driver] {
		writeJSON(w, 400, map[string]string{"error": "driver does not advertise pv-curtail support"})
		return
	}
	if len(supports) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no drivers advertise pv-curtail support"})
		return
	}

	// Snapshot the driver config blocks too so the pct conversion can
	// reach for nominal_w. Done outside resolvePVLimitW to keep that
	// function pure for tests.
	cfgDrivers := s.snapshotDriverConfigs()
	limitW, err := resolvePVLimitW(req, supports, s.deps.Tel, cfgDrivers)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if limitW < 0 {
		limitW = 0
	}

	expires := time.Now().Add(time.Duration(req.HoldS) * time.Second)
	hold := control.PVManualHold{
		Driver:    req.Driver,
		LimitW:    limitW,
		ExpiresAt: expires,
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetPVManualHold(hold)
	s.deps.CtrlMu.Unlock()

	slog.Info("pv manual hold installed",
		"driver", req.Driver,
		"limit_w", limitW,
		"hold_s", req.HoldS,
	)
	writeJSON(w, 200, pvManualHoldResponseFrom(hold, true))
}

func (s *Server) handlePVManualHoldClear(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.ClearPVManualHold()
	s.deps.CtrlMu.Unlock()
	slog.Info("pv manual hold cleared")
	writeJSON(w, 200, pvManualHoldResponse{Active: false})
}

func (s *Server) handlePVManualHoldGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, 503, map[string]string{"error": "control state not available"})
		return
	}
	s.deps.CtrlMu.Lock()
	h, active := s.deps.Ctrl.GetPVManualHold(time.Now())
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, pvManualHoldResponseFrom(h, active))
}

// resolvePVLimitW converts the request to an absolute watt cap.
//
// limit_pct is interpreted as a fraction of the driver's configured
// nominal_w (the inverter's rated AC output) — that's what an operator
// means by "100%" when they look at the planet bubble. Falls back to
// live |PV| if no nominal_w is configured (better something than a
// hard error, but the slider becomes effectively meaningless above
// current production).
//
// `cfgDrivers` maps driver name → nominal_w in watts (0 = not set).
func resolvePVLimitW(req pvManualHoldRequest, supports map[string]bool, tel *telemetry.Store, cfgDrivers map[string]float64) (float64, error) {
	if req.LimitW != nil {
		if *req.LimitW < 0 {
			return 0, apiError("limit_w must be >= 0")
		}
		return *req.LimitW, nil
	}
	pct := *req.LimitPct
	if pct < 0 || pct > 100 {
		return 0, apiError("limit_pct must be in [0, 100]")
	}

	// Preferred basis: nominal_w. Driver-scoped uses that one driver's
	// nominal; aggregate sums the nominal_w of every curtail-supporting
	// driver.
	var basis float64
	if req.Driver != "" {
		basis = cfgDrivers[req.Driver]
	} else {
		for d := range supports {
			basis += cfgDrivers[d]
		}
	}
	if basis > 0 {
		return basis * pct / 100.0, nil
	}

	// Fallback: live |PV|. Same sum semantics as before.
	if tel == nil {
		return 0, apiError("nominal_w not configured and telemetry unavailable")
	}
	for _, r := range tel.ReadingsByType(telemetry.DerPV) {
		if req.Driver != "" {
			if r.Driver != req.Driver {
				continue
			}
		} else if !supports[r.Driver] {
			continue
		}
		if r.RawW >= 0 {
			continue
		}
		basis += -r.RawW
	}
	if basis <= 0 {
		// 0 % of 0 W is 0 W — valid "force off" verification command.
		return 0, nil
	}
	return basis * pct / 100.0, nil
}

// snapshotDriverConfigs walks Cfg.Drivers and pulls each driver's
// nominal_w out of its `config:` block under the CfgMu read lock.
// Returns name → nominal_w (0 when missing or wrong type).
func (s *Server) snapshotDriverConfigs() map[string]float64 {
	out := map[string]float64{}
	if s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return out
	}
	s.deps.CfgMu.RLock()
	defer s.deps.CfgMu.RUnlock()
	for _, d := range s.deps.Cfg.Drivers {
		out[d.Name] = readNominalW(d)
	}
	return out
}

// readNominalW pulls nominal_w out of the YAML map; accepts numeric
// types (int, int64, float64) since YAML loaders are inconsistent.
func readNominalW(d config.Driver) float64 {
	v, ok := d.Config["nominal_w"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func pvManualHoldResponseFrom(h control.PVManualHold, active bool) pvManualHoldResponse {
	resp := pvManualHoldResponse{Active: active}
	if !active {
		return resp
	}
	resp.Driver = h.Driver
	resp.LimitW = h.LimitW
	if !h.ExpiresAt.IsZero() {
		resp.ExpiresAtMs = h.ExpiresAt.UnixMilli()
	}
	return resp
}
