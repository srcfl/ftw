package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/sitecontroller"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type siteControllerPairingOffer struct {
	Pairing sitecontroller.PairingEnvelope `json:"pairing"`
	Scopes  []string                       `json:"scopes"`
}

// handleSiteControllerPairing mints a short-lived, signed pairing proof only
// after an explicit local POST. It does not contact Nova or persist a grant.
func (s *Server) handleSiteControllerPairing(w http.ResponseWriter, r *http.Request) {
	// A non-simple header forces browser origins through CORS preflight; this
	// private endpoint intentionally provides no CORS grant. Native mobile and
	// the same-origin FTW UI set it only after an explicit user action.
	if r.Header.Get("X-FTW-Pairing-Intent") != "pair" {
		writeSiteControllerJSON(w, http.StatusForbidden, map[string]string{"error": "explicit pairing intent required"})
		return
	}
	if s.deps.SiteControllerIdentity == nil {
		writeSiteControllerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site controller identity unavailable"})
		return
	}
	anchor, err := s.discoveredZapAnchor()
	if err != nil {
		writeSiteControllerJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not inspect local Zap identity"})
		return
	}
	offer, err := sitecontroller.NewPairing(s.deps.SiteControllerIdentity, anchor, time.Now().UTC(), nil)
	if err != nil {
		writeSiteControllerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not create pairing proof"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeSiteControllerJSON(w, http.StatusOK, siteControllerPairingOffer{
		Pairing: *offer,
		Scopes:  sitecontroller.ReadScopes(),
	})
}

// handleSiteControllerSnapshot returns only controller status, aggregate driver
// health, and a bounded plan preview. Raw energy measurements remain owned by
// Zap/device/DER telemetry and are intentionally absent from this contract.
func (s *Server) handleSiteControllerSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.deps.SiteControllerIdentity == nil || s.deps.Tel == nil || s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeSiteControllerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site controller snapshot unavailable"})
		return
	}
	siteID := r.URL.Query().Get("site_id")
	if siteID == "" {
		writeSiteControllerJSON(w, http.StatusBadRequest, map[string]string{"error": "site_id is required"})
		return
	}
	now := time.Now().UTC()
	status := s.siteControllerStatus()
	health := s.siteControllerHealth()
	plan := s.siteControllerPlan(now)
	envelope, err := sitecontroller.NewSnapshot(s.deps.SiteControllerIdentity, siteID, status, health, plan, now, nil)
	if err != nil {
		writeSiteControllerJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeSiteControllerJSON(w, http.StatusOK, envelope)
}

func writeSiteControllerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) discoveredZapAnchor() (*string, error) {
	if s.deps.State == nil {
		return nil, nil
	}
	devices, err := s.deps.State.AllDevices()
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		serial := strings.TrimSpace(device.Serial)
		if !strings.HasPrefix(serial, "zap-") {
			continue
		}
		if strings.EqualFold(device.Make, "Sourceful") || device.DriverName == "sourceful-zap" {
			return &serial, nil
		}
	}
	return nil, nil
}

func (s *Server) siteControllerStatus() sitecontroller.StatusSnapshot {
	s.deps.CtrlMu.Lock()
	mode := string(s.deps.Ctrl.Mode)
	planStale := s.deps.Ctrl.PlanStale
	s.deps.CtrlMu.Unlock()
	return sitecontroller.StatusSnapshot{
		SoftwareVersion: s.deps.Version,
		Mode:            mode,
		PlanStale:       planStale,
	}
}

func (s *Server) siteControllerHealth() sitecontroller.HealthSnapshot {
	var ok, degraded, offline, faulted int
	for _, health := range s.deps.Tel.AllHealth() {
		if health.DeviceFault {
			faulted++
			continue
		}
		switch health.Status {
		case telemetry.StatusOk:
			ok++
		case telemetry.StatusDegraded:
			degraded++
		case telemetry.StatusOffline:
			offline++
		}
	}
	state := "ok"
	if offline > 0 || faulted > 0 {
		state = "degraded"
	}
	return sitecontroller.HealthSnapshot{
		State:           state,
		DriversOK:       ok,
		DriversDegraded: degraded,
		DriversOffline:  offline,
		DriversFaulted:  faulted,
	}
}

func (s *Server) siteControllerPlan(now time.Time) sitecontroller.PlanSnapshot {
	result := sitecontroller.PlanSnapshot{Enabled: s.deps.MPC != nil, Actions: []sitecontroller.PlanAction{}}
	if s.deps.MPC == nil {
		return result
	}
	plan := s.deps.MPC.Latest()
	if plan == nil {
		return result
	}
	result.GeneratedAtMS = plan.GeneratedAtMs
	result.Mode = string(plan.Mode)
	for _, action := range plan.Actions {
		endMS := action.SlotStartMs + int64(action.SlotLenMin)*time.Minute.Milliseconds()
		if endMS <= now.UnixMilli() {
			continue
		}
		result.ActionCount++
		if len(result.Actions) >= sitecontroller.MaxActions {
			continue
		}
		result.Actions = append(result.Actions, sitecontroller.PlanAction{
			StartMS:  action.SlotStartMs,
			EndMS:    endMS,
			BatteryW: int(math.Round(action.BatteryW)),
			GridW:    int(math.Round(action.GridW)),
		})
	}
	return result
}
