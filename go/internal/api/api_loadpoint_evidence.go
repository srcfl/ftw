package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// chargingEvidence joins the same controller view the UI reads with a fresh
// charger observation. These are separate reads, not an atomic site snapshot.
// CommandedW and CommandedReason describe Core's last decision, not a driver
// acknowledgement. Observed power alone does not prove a command caused it.
type chargingEvidence struct {
	SchemaVersion     int                 `json:"schema_version"`
	CollectedAtMs     int64               `json:"collected_at_ms"`
	ReadCompletedAtMs int64               `json:"read_completed_at_ms"`
	Loadpoint         loadpoint.State     `json:"loadpoint"`
	PlanStatus        string              `json:"plan_status"`
	PlanGeneratedAtMs int64               `json:"plan_generated_at_ms,omitempty"`
	Observation       chargingObservation `json:"observation"`
}

type chargingObservation struct {
	// Carrier is the driver's telemetry freshness; SrcState is the power
	// source's freshness. A cached cloud value can have a live carrier.
	Carrier           string   `json:"carrier"`
	SrcState          string   `json:"srcState"`
	ReceivedAtMs      int64    `json:"received_at_ms,omitempty"`
	PowerObservedAtMs int64    `json:"power_observed_at_ms,omitempty"`
	PowerMaxAgeMs     int64    `json:"power_max_age_ms,omitempty"`
	Connected         *bool    `json:"connected"`
	PowerW            *float64 `json:"power_w"`
	ChargerLimitA     *float64 `json:"charger_limit_a"`
	// State describes an observation, never inferred battery completion.
	State string `json:"state"`
}

// loadpointStates is shared by the ordinary UI read and charging evidence.
// Keep the manager's estimate separate from the vehicle's reported SoC.
func (s *Server) loadpointStates() ([]loadpoint.State, mpc.PlanSnapshot) {
	if s.deps.Loadpoints == nil {
		return []loadpoint.State{}, mpc.PlanSnapshot{}
	}
	states := s.deps.Loadpoints.States()
	if s.deps.Tel != nil {
		decorateLoadpointsWithVehicle(states, s.deps.Tel)
	}
	s.decorateLoadpointsWithManual(states)
	s.decorateLoadpointsWithBatteryBoost(states)
	plan := s.decorateLoadpointsWithPlan(states)
	return states, plan
}

func (s *Server) loadpointEvidence(id string) (chargingEvidence, bool) {
	started := time.Now()
	states, plan := s.loadpointStates()
	for _, st := range states {
		if st.ID != id {
			continue
		}
		var rd *telemetry.DerReading
		var health *telemetry.DriverHealth
		if s.deps.Tel != nil {
			rd = s.deps.Tel.Get(st.DriverName, telemetry.DerEV)
			health = s.deps.Tel.DriverHealth(st.DriverName)
		}
		watchdog := time.Minute
		if s.deps.Cfg != nil && s.deps.CfgMu != nil {
			s.deps.CfgMu.RLock()
			if s.deps.Cfg.Site.WatchdogTimeoutS > 0 {
				watchdog = time.Duration(s.deps.Cfg.Site.WatchdogTimeoutS) * time.Second
			}
			s.deps.CfgMu.RUnlock()
		}
		if health != nil && health.WatchdogTimeoutOverride > 0 {
			watchdog = health.WatchdogTimeoutOverride
		}
		now := time.Now()
		evidence := chargingEvidence{
			SchemaVersion: 1, CollectedAtMs: started.UnixMilli(), ReadCompletedAtMs: now.UnixMilli(),
			Loadpoint: st, Observation: chargingObservationFrom(rd, health, watchdog, now),
			PlanStatus: "missing",
		}
		if plan.Plan != nil {
			evidence.PlanGeneratedAtMs = plan.Plan.GeneratedAtMs
			evidence.PlanStatus = "current"
			generated := time.UnixMilli(plan.Plan.GeneratedAtMs)
			if plan.Outdated || plan.Plan.GeneratedAtMs <= 0 || generated.After(now.Add(time.Second)) || now.Sub(generated) > mpc.MaxPlanAge {
				evidence.PlanStatus = "outdated"
			}
		}
		if plan.Pending {
			evidence.PlanStatus = "pending"
		}
		return evidence, true
	}
	return chargingEvidence{}, false
}

func (s *Server) handleLoadpointEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	evidence, ok := s.loadpointEvidence(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "loadpoint not found"})
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

// toolChargingEvidence returns one bounded view. With no id it lists IDs so
// the model need not guess configuration names or fetch every site's data.
func (s *Server) toolChargingEvidence(args json.RawMessage) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid charging evidence arguments: %w", err)
	}
	if in.ID == "" {
		ids := []string{}
		truncated := false
		if s.deps.Loadpoints != nil {
			for _, cfg := range s.deps.Loadpoints.Configs() {
				if len(ids) == 32 {
					truncated = true
					break
				}
				ids = append(ids, cfg.ID)
			}
		}
		b, err := json.Marshal(struct {
			IDs       []string `json:"loadpoint_ids"`
			Truncated bool     `json:"truncated"`
		}{ids, truncated})
		return string(b), err
	}
	evidence, ok := s.loadpointEvidence(in.ID)
	if !ok {
		return "", fmt.Errorf("loadpoint not found")
	}
	b, err := json.Marshal(evidence)
	return string(b), err
}

func chargingObservationFrom(rd *telemetry.DerReading, health *telemetry.DriverHealth, watchdog time.Duration, now time.Time) chargingObservation {
	out := chargingObservation{Carrier: "unknown", SrcState: "unknown", State: "unknown"}
	if rd == nil || rd.UpdatedAt.IsZero() {
		return out
	}
	out.ReceivedAtMs = rd.UpdatedAt.UnixMilli()
	if watchdog <= 0 {
		watchdog = time.Minute
	}
	if rd.UpdatedAt.After(now.Add(time.Second)) || now.Sub(rd.UpdatedAt) > watchdog ||
		(health != nil && !health.TelemetryLive()) {
		out.Carrier = "stale"
		return out
	}
	out.Carrier = "live"
	var data struct {
		Connected         *bool    `json:"connected"`
		ConnectionUnknown bool     `json:"connection_unknown"`
		Online            *bool    `json:"is_online"`
		MaxA              *float64 `json:"max_a"`
		PowerAt           string   `json:"power_observed_at"`
		PowerMaxAgeS      int      `json:"power_max_age_s"`
	}
	if json.Unmarshal(rd.Data, &data) != nil || (data.Online != nil && !*data.Online) {
		return out
	}
	if !data.ConnectionUnknown {
		out.Connected = data.Connected
	}
	if data.MaxA != nil && *data.MaxA >= 0 && !math.IsInf(*data.MaxA, 0) && !math.IsNaN(*data.MaxA) {
		out.ChargerLimitA = data.MaxA
	}
	at := rd.UpdatedAt
	if data.PowerAt != "" {
		var err error
		at, err = time.Parse(time.RFC3339Nano, data.PowerAt)
		if err != nil {
			return out
		}
	}
	out.PowerObservedAtMs = at.UnixMilli()
	// Use the same source cadence bound as loadpoint energy accounting.
	sample := loadpoint.EVSample{PowerMaxAge: time.Duration(min(max(data.PowerMaxAgeS, 0), 180)) * time.Second}
	out.PowerMaxAgeMs = sample.PowerWindow().Milliseconds()
	if at.After(now.Add(time.Second)) || now.Sub(at) > sample.PowerWindow() {
		out.SrcState = "stale"
		return out
	}
	if math.IsNaN(rd.RawW) || math.IsInf(rd.RawW, 0) || rd.RawW < 0 {
		return out
	}
	out.SrcState = "live"
	w := rd.RawW
	out.PowerW = &w
	switch {
	case out.Connected == nil:
		// Power can be measured while connection identity is unknown.
	case !*out.Connected && w >= loadpoint.DeliveringW:
		// Contradicting readings cannot prove either charging or unplugged.
	case !*out.Connected:
		out.State = "disconnected"
	case w >= loadpoint.DeliveringW:
		out.State = "charging"
	default:
		out.State = "not_drawing"
	}
	return out
}
