package api

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// ControlFeedback is read-only evidence for every mode. Missing numbers are
// null, never invented zeros. The client owns prose; reasons describe only
// what Core or the device actually reported, not a guessed physical cause.
type ControlFeedback struct {
	SiteEvidence     *ControlComparison `json:"site_evidence,omitempty"`
	SiteBeforeW      *float64           `json:"site_before_w"`
	SiteAfterW       *float64           `json:"site_after_w"`
	SiteBeforeAtMs   int64              `json:"site_before_at_ms,omitempty"`
	SiteAfterAtMs    int64              `json:"site_after_at_ms,omitempty"`
	ToleranceW       *float64           `json:"tolerance_w"`
	VerificationTier *int               `json:"verification_tier"`
	VerificationLost bool               `json:"verification_lost,omitempty"`
	SiteSourceIssue  string             `json:"site_source_issue,omitempty"`
	SiteConfirmation string             `json:"site_confirmation"`
	SiteMeter        string             `json:"site_meter,omitempty"`
	SiteDeltaW       *float64           `json:"site_delta_w"`
	DeviceDeltaW     *float64           `json:"device_delta_w"`
	Response         string             `json:"response"`
	VerifiedAtMs     int64              `json:"verified_at_ms,omitempty"`
	Driver           string             `json:"driver"`
	Kind             string             `json:"kind"`
	Mode             string             `json:"mode"`
	State            string             `json:"state"`
	Reason           string             `json:"reason"`
	Severity         string             `json:"severity"`
	RequestedW       *float64           `json:"requested_w"`
	SentW            *float64           `json:"sent_w"`
	ReadbackW        *float64           `json:"readback_w"`
	ActualW          *float64           `json:"actual_w"`
	RequestedA       *float64           `json:"requested_a"`
	OfferedA         *float64           `json:"offered_a"`
	DeviceLimitA     *float64           `json:"device_limit_a"`
	DeviceReason     string             `json:"device_reason,omitempty"`
	SinceMs          int64              `json:"since_ms,omitempty"`
	CommandAtMs      int64              `json:"command_at_ms,omitempty"`
	ObservedAtMs     int64              `json:"observed_at_ms,omitempty"`
}

type feedbackReading struct {
	SetpointW       *float64 `json:"setpoint_w"`
	MaxA            *float64 `json:"max_a"`
	DeviceLimitA    *float64 `json:"device_limit_a"`
	DeviceLimitAgeS *float64 `json:"device_limit_age_s"`
	Reason          string   `json:"reason_no_current_label"`
	Connected       *bool    `json:"connected"`
	Online          *bool    `json:"is_online"`
}

func watts(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func (s *Server) controlFeedback(now time.Time) []ControlFeedback {
	out := []ControlFeedback{}
	if s.deps.Tel == nil {
		return out
	}
	mode, meter := "", ""
	clamped := map[string]bool{}
	targeted := map[string]bool{}
	var hold control.BatteryManualHold
	var held bool
	if s.deps.Ctrl != nil && s.deps.CtrlMu != nil {
		s.deps.CtrlMu.Lock()
		mode = string(s.deps.Ctrl.Mode)
		meter = s.deps.Ctrl.SiteMeterDriver
		for _, t := range s.deps.Ctrl.LastTargets {
			clamped[t.Driver] = t.Clamped
			targeted[t.Driver] = true
		}
		hold, held = s.deps.Ctrl.GetBatteryManualHold(now)
		s.deps.CtrlMu.Unlock()
	}
	observe, disabled := map[string]bool{}, map[string]bool{}
	configuredBattery := map[string]bool{}
	if s.deps.Cfg != nil && s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		observe = config.ObserveOnlyDriverSet(s.deps.Cfg)
		for _, d := range s.deps.Cfg.Drivers {
			disabled[d.Name] = d.Disabled
			configuredBattery[d.Name] = d.BatteryCapacityWh > 0 && !d.BatteryTelemetryOnly
		}
		s.deps.CfgMu.RUnlock()
	}
	lps := map[string]loadpoint.State{}
	if s.deps.Loadpoints != nil {
		states := s.deps.Loadpoints.States()
		s.decorateLoadpointsWithManual(states)
		for _, lp := range states {
			lps[lp.DriverName] = lp
		}
	}
	blocked := ""
	if s.deps.SiteDispatchBlocked != nil {
		blocked = s.deps.SiteDispatchBlocked()
	}
	for _, kind := range []telemetry.DerType{telemetry.DerBattery, telemetry.DerEV, telemetry.DerPV, telemetry.DerV2X} {
		for _, rd := range s.deps.Tel.ReadingsByType(kind) {
			cmd, commanded := s.deps.Tel.CommandEvidence(rd.Driver, kind.String())
			_, loadpointOwned := lps[rd.Driver]
			manualBattery := held && kind == telemetry.DerBattery && (hold.Driver == "" || hold.Driver == rd.Driver)
			// Measurements inform the site comparison, but only controlled
			// functions receive a command result. PV becomes one on curtailment.
			if !commanded && !(kind == telemetry.DerBattery && (targeted[rd.Driver] || configuredBattery[rd.Driver])) && !(kind == telemetry.DerEV && loadpointOwned) && !manualBattery {
				continue
			}
			if kind == telemetry.DerPV && !commanded {
				continue
			}
			f := ControlFeedback{Driver: rd.Driver, Kind: kind.String(), Mode: mode, State: "waiting", Reason: "no_command", Severity: "info", Response: "unconfirmed"}
			var d feedbackReading
			_ = json.Unmarshal(rd.Data, &d)
			f.ReadbackW, f.OfferedA, f.DeviceReason = d.SetpointW, d.MaxA, d.Reason
			if d.DeviceLimitAgeS != nil && *d.DeviceLimitAgeS >= 0 && *d.DeviceLimitAgeS <= 120 {
				f.DeviceLimitA = d.DeviceLimitA
			}
			f.ObservedAtMs = rd.UpdatedAt.UnixMilli()
			fresh := now.Sub(rd.UpdatedAt) <= time.Minute && !rd.UpdatedAt.After(now)
			if h := s.deps.Tel.DriverHealth(rd.Driver); h != nil {
				fresh = fresh && h.TelemetryLive()
			}
			if d.Online != nil && !*d.Online {
				fresh = false
			}
			measurement, powerKnown := telemetry.ControlPowerObservation(rd.RawW, rd.Data, rd.UpdatedAt)
			if !measurement.At.IsZero() {
				f.ObservedAtMs = measurement.At.UnixMilli()
			}
			if fresh && powerKnown && now.Sub(measurement.At) <= time.Minute {
				f.ActualW = watts(measurement.PowerW)
			}

			if commanded {
				f.SentW, f.RequestedW = cmd.PowerW, cmd.PowerW
				if cmd.PowerW != nil {
					f.ToleranceW = watts(telemetry.ControlToleranceW(*cmd.PowerW))
				}
				f.CommandAtMs, f.SinceMs = cmd.At.UnixMilli(), cmd.Since.UnixMilli()
				if cmd.Result == "accepted" {
					tier := 0
					f.VerificationTier = &tier
				}
			}
			coreReason := ""
			if clamped[rd.Driver] && kind == telemetry.DerBattery {
				coreReason = "core_limit"
			}
			if held && kind == telemetry.DerBattery && (hold.Driver == "" || hold.Driver == rd.Driver) {
				f.Mode = "manual"
				f.RequestedW = watts(hold.PowerW)
			}
			if lp, ok := lps[rd.Driver]; ok && kind == telemetry.DerEV {
				f.Mode = "plan"
				if lp.SurplusOnly {
					f.Mode = "solar"
				}
				if lp.ManualActive {
					f.Mode = "manual"
				}
				if lp.CommandedKnown {
					f.RequestedW = watts(lp.CommandedW)
					coreReason = lp.CommandedReason
				}
				if lp.ManualActive {
					f.RequestedW = watts(lp.ManualChargeW)
				}
				if f.RequestedW != nil && lp.Phases > 0 && lp.VoltageV > 0 {
					f.RequestedA = watts(*f.RequestedW / float64(lp.Phases) / lp.VoltageV)
				}
				if lp.PowerUnavailable {
					fresh = false
					f.ActualW = nil
				}
			}
			if commanded && cmd.Result == "accepted" && fresh && f.ActualW != nil && f.SentW != nil && telemetry.PowerFollowsCommand(f.Kind, *f.SentW, *f.ActualW) && !cmd.PowerMatchSince.IsZero() && cmd.LastObservation.Sub(cmd.PowerMatchSince) >= 10*time.Second && now.Sub(cmd.LastObservation) <= 10*time.Second {
				f.Response = "device_reported"
				tier := 1
				f.VerificationTier = &tier
				f.VerifiedAtMs = cmd.LastObservation.UnixMilli()
			}
			f.SiteConfirmation = "device_response_unconfirmed"
			if f.VerificationTier != nil && *f.VerificationTier == 1 {
				from := now.Add(-telemetry.ControlWindowDuration)
				if cmd.Since.After(from) {
					from = cmd.Since
				}
				f.SiteMeter = meter
				after := s.deps.Tel.ControlWindows(from, now)
				comparison := s.controlResponse(cmd, meter, after, now)
				f.SiteEvidence = &comparison
				f.SiteConfirmation, f.DeviceDeltaW, f.SiteDeltaW = comparison.Reason, comparison.DeviceDeltaW, comparison.SiteDeltaW
				if f.SiteDeltaW != nil {
					f.SiteBeforeW, f.SiteAfterW = watts(comparison.BeforeW), watts(comparison.AfterW)
					f.SiteBeforeAtMs, f.SiteAfterAtMs = comparison.BeforeAt.UnixMilli(), comparison.AfterAt.UnixMilli()
				}
				if h := s.deps.Tel.DriverHealth(meter); h == nil || !h.TelemetryLive() || h.DeviceFault || blocked != "" {
					f.SiteConfirmation = "waiting_for_meter"
					f.SiteDeltaW, f.SiteBeforeW, f.SiteAfterW = nil, nil, nil
					f.SiteEvidence = nil
				}
				if f.SiteConfirmation == "confirmed" {
					tier := 2
					f.VerificationTier = &tier
					f.Response = "site_confirmed"
				}
			}
			classifyControlFeedback(&f, cmd, commanded, fresh, coreReason, d.Connected, now)
			if h := s.deps.Tel.DriverHealth(rd.Driver); h != nil && h.DeviceFault {
				f.State, f.Reason, f.Severity = "blocked", "device_fault", "warning"
				f.DeviceReason = h.DeviceFaultReason
			}
			if s.deps.Registry != nil {
				if cs, ok := s.deps.Registry.ControlStatus(rd.Driver); ok && cs.Blocked {
					f.State, f.Reason, f.Severity = "blocked", "default_failed", "warning"
				}
			}
			if blocked != "" {
				f.State, f.Reason, f.Severity = "blocked", blocked, "warning"
			}
			if observe[rd.Driver] {
				f.State, f.Reason, f.Severity = "observing", "observe_only", "info"
			}
			if disabled[rd.Driver] {
				f.State, f.Reason, f.Severity = "blocked", "disabled", "info"
			}
			if observe[rd.Driver] || disabled[rd.Driver] {
				f.VerificationTier, f.SiteEvidence, f.Response = nil, nil, "unconfirmed"
				f.SiteConfirmation = "not_controlling"
				f.SiteDeltaW, f.DeviceDeltaW, f.SiteBeforeW, f.SiteAfterW = nil, nil, nil, nil
				f.SiteBeforeAtMs, f.SiteAfterAtMs = 0, 0
			}
			f.VerificationLost = controlVerificationLost(f, cmd, now)
			if f.VerificationLost {
				f.Severity = "warning"
			}
			out = append(out, f)
		}
	}
	seen := map[string]bool{}
	for _, f := range out {
		seen[f.Driver] = true
	}
	for driver, lp := range lps {
		if seen[driver] {
			continue
		}
		mode := "plan"
		if lp.ManualActive {
			mode = "manual"
		}
		if lp.SurplusOnly {
			mode = "solar"
		}
		out = append(out, ControlFeedback{Driver: driver, Kind: "ev", Mode: mode, State: "unknown", Reason: "telemetry_stale", Severity: "warning"})
		seen[driver] = true
	}
	for driver, h := range s.deps.Tel.AllHealth() {
		if seen[driver] || !h.DeviceFault {
			continue
		}
		controlled := targeted[driver]
		for _, kind := range []string{"battery", "ev", "pv", "v2x_charger"} {
			if c, ok := s.deps.Tel.CommandEvidence(driver, kind); ok && c.Result != "released" {
				controlled = true
			}
		}
		if !controlled {
			continue
		}
		out = append(out, ControlFeedback{Driver: driver, Kind: "device", Mode: mode, State: "blocked", Reason: "device_fault", Severity: "warning", DeviceReason: h.DeviceFaultReason})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Driver == out[j].Driver {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Driver < out[j].Driver
	})
	return out
}

// An acknowledgement is not an alarm during the normal response wait. A
// function that previously supplied measured proof must recover that proof.
// This state comes from telemetry, not from whether a browser was open.
func controlVerificationLost(f ControlFeedback, cmd telemetry.CommandEvidence, now time.Time) bool {
	if cmd.LastVerifiedAt.IsZero() || f.VerificationTier != nil && *f.VerificationTier >= 1 {
		return false
	}
	switch f.Reason {
	case "observe_only", "disabled", "device_control", "not_connected", "idle", "no_command":
		return false
	}
	if cmd.Result == "released" {
		return false
	}
	if f.Reason == "telemetry_stale" || f.Reason == "device_fault" || f.Reason == "command_failed" || f.Reason == "default_failed" {
		return true
	}
	grace := 30 * time.Second
	if f.Kind == "ev" {
		grace = 2 * time.Minute
	}
	return !cmd.Since.IsZero() && now.Sub(cmd.Since) >= grace && now.Sub(cmd.LastVerifiedAt) >= grace
}

func classifyControlFeedback(f *ControlFeedback, cmd telemetry.CommandEvidence, commanded, fresh bool, coreReason string, connected *bool, now time.Time) {
	set := func(state, reason, severity string) { f.State, f.Reason, f.Severity = state, reason, severity }
	if !fresh {
		f.ActualW, f.ReadbackW, f.OfferedA, f.DeviceLimitA = nil, nil, nil, nil
		set("unknown", "telemetry_stale", "warning")
		return
	}
	if commanded && cmd.Result == "default_failed" {
		set("blocked", "default_failed", "warning")
		return
	}
	if commanded && cmd.Result == "released" {
		set("idle", "device_control", "info")
		return
	}
	if commanded && cmd.Result == "failed" {
		set("blocked", "command_failed", "warning")
		return
	}
	if commanded && cmd.Result == "unconfirmed" {
		set("unknown", "command_unconfirmed", "warning")
		return
	}
	if f.Kind == "ev" && connected != nil && !*connected {
		set("waiting", "not_connected", "info")
		return
	}
	if f.RequestedA != nil && f.DeviceLimitA != nil && *f.RequestedA > *f.DeviceLimitA+0.5 {
		set("limited", "device_limit", "warning")
		return
	}
	if coreReason == "site_meter_stale" || coreReason == "site_phase_currents_stale" || coreReason == "fuse_cooldown" || coreReason == "fuse_limit" || coreReason == "charger_limit" {
		set("limited", coreReason, "warning")
		return
	}
	if !commanded {
		if coreReason != "" {
			set("waiting", coreReason, "info")
		}
		return
	}
	grace := 30 * time.Second
	if f.Kind == "ev" {
		grace = 2 * time.Minute
	}
	settled := !cmd.Since.IsZero() && now.Sub(cmd.Since) >= grace
	if !cmd.ReadbackMismatchSince.IsZero() && now.Sub(cmd.ReadbackMismatchSince) >= 30*time.Second && f.ReadbackW != nil {
		set("warning", "setpoint_changed", "warning")
		return
	}
	if f.Kind == "ev" && f.SentW != nil && f.RequestedA != nil && f.OfferedA != nil && *f.SentW > 100 && *f.OfferedA+0.5 < *f.RequestedA && settled {
		set("limited", "offered_current_lower", "warning")
		return
	}
	if cmd.Result == "pending" || (!settled && f.Response == "unconfirmed") || f.ObservedAtMs < cmd.Since.UnixMilli() {
		set("waiting", "waiting_response", "info")
		return
	}
	if f.SentW == nil || f.ActualW == nil {
		set("unknown", "response_unknown", "info")
		return
	}
	gap := math.Abs(*f.SentW - *f.ActualW)
	// A PV command is a ceiling; producing less is expected in weak sun.
	if f.Kind == "pv" {
		gap = math.Max(0, math.Abs(*f.ActualW)-math.Abs(*f.SentW))
	}
	if !cmd.PowerMismatchSince.IsZero() && now.Sub(cmd.PowerMismatchSince) >= grace && gap > telemetry.ControlToleranceW(*f.SentW) {
		set("warning", "power_differs", "warning")
		return
	}
	if coreReason == "core_limit" {
		set("limited", "core_limit", "info")
		return
	}
	if math.Abs(*f.SentW) < 100 {
		if coreReason == "" || coreReason == "manual_hold" {
			coreReason = "idle"
		}
		set("idle", coreReason, "info")
		return
	}
	if gap > telemetry.ControlToleranceW(*f.SentW) {
		set("waiting", "waiting_response", "info")
		return
	}
	if f.Response != "device_reported" && f.Response != "site_confirmed" {
		if f.Kind == "pv" && settled && math.Abs(*f.ActualW) < math.Abs(*f.SentW)-telemetry.ControlToleranceW(*f.SentW) {
			set("unknown", "solar_below_ceiling", "info")
			return
		}
		set("waiting", "waiting_response", "info")
		return
	}
	set("following", "power_observed", "info")
}

// Keep status and the EV panel on the same explanation. This wrapper adds
// evidence to the existing endpoint without changing stored loadpoint state.
type loadpointFeedbackView struct {
	loadpoint.State
	ControlFeedback *ControlFeedback `json:"control_feedback,omitempty"`
}

func (s *Server) loadpointsWithFeedback(states []loadpoint.State) []loadpointFeedbackView {
	feedback := s.controlFeedback(time.Now())
	byDriver := map[string]*ControlFeedback{}
	for i := range feedback {
		if feedback[i].Kind == "ev" {
			byDriver[feedback[i].Driver] = &feedback[i]
		}
	}
	out := make([]loadpointFeedbackView, 0, len(states))
	for _, st := range states {
		out = append(out, loadpointFeedbackView{st, byDriver[st.DriverName]})
	}
	return out
}
