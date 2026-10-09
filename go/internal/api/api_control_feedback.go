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

// ControlFeedback answers "Are we in control?" for one device function in
// every mode. Status and severity are Core's verdict; clients choose words and
// colours for them and never derive their own. Evidence names the strongest
// proof behind it: accepted (the driver took the call), measured (fresh device
// readings) or confirmed (a separate meter saw the change). Missing numbers
// are null, never invented zeros. Reasons describe only what Core or the
// device reported, not a guessed physical cause.
type ControlFeedback struct {
	Driver           string             `json:"driver"`
	Kind             string             `json:"kind"`
	Mode             string             `json:"mode"`
	Status           string             `json:"status"`
	Reason           string             `json:"reason"`
	Severity         string             `json:"severity"`
	Evidence         string             `json:"evidence"`
	ReadingsFresh    bool               `json:"readings_fresh"`
	ConfirmedAtMs    int64              `json:"confirmed_at_ms,omitempty"`
	SiteConfirmation string             `json:"site_confirmation"`
	SiteMeter        string             `json:"site_meter,omitempty"`
	SiteEvidence     *ControlComparison `json:"site_evidence,omitempty"`
	ToleranceW       *float64           `json:"tolerance_w"`
	// Internal steps of the verdict; the API exposes their result above.
	State            string   `json:"-"`
	VerificationTier *int     `json:"-"`
	VerificationLost bool     `json:"-"`
	Response         string   `json:"-"`
	LimitedBy        string   `json:"-"`
	SiteBeforeW      *float64 `json:"-"`
	SiteAfterW       *float64 `json:"-"`
	SiteBeforeAtMs   int64    `json:"-"`
	SiteAfterAtMs    int64    `json:"-"`
	SiteDeltaW       *float64 `json:"-"`
	DeviceDeltaW     *float64 `json:"-"`
	VerifiedAtMs     int64    `json:"-"`
	RequestedW       *float64 `json:"requested_w"`
	SentW            *float64 `json:"sent_w"`
	ReadbackW        *float64 `json:"readback_w"`
	ActualW          *float64 `json:"actual_w"`
	BatterySoC       *float64 `json:"battery_soc,omitempty"`
	ChargeResumeSoC  *float64 `json:"charge_resume_soc,omitempty"`
	RequestedA       *float64 `json:"requested_a"`
	OfferedA         *float64 `json:"offered_a"`
	DeviceLimitA     *float64 `json:"device_limit_a"`
	DeviceReason     string   `json:"device_reason,omitempty"`
	SinceMs          int64    `json:"since_ms,omitempty"`
	CommandAtMs      int64    `json:"command_at_ms,omitempty"`
	ObservedAtMs     int64    `json:"observed_at_ms,omitempty"`
}

type feedbackReading struct {
	SetpointW       *float64 `json:"setpoint_w"`
	MaxA            *float64 `json:"max_a"`
	DeviceLimitA    *float64 `json:"device_limit_a"`
	DeviceLimitAgeS *float64 `json:"device_limit_age_s"`
	Reason          string   `json:"reason_no_current_label"`
	LimitedBy       string   `json:"current_limited_by"`
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
	full := map[string]bool{}
	targeted := map[string]bool{}
	var hold control.BatteryManualHold
	var held bool
	if s.deps.Ctrl != nil && s.deps.CtrlMu != nil {
		s.deps.CtrlMu.Lock()
		mode = string(s.deps.Ctrl.Mode)
		meter = s.deps.Ctrl.SiteMeterDriver
		for _, t := range s.deps.Ctrl.LastTargets {
			clamped[t.Driver] = t.Clamped
			full[t.Driver] = s.deps.Ctrl.BatteryChargePaused(t.Driver)
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
			f.ReadbackW, f.OfferedA, f.DeviceReason, f.LimitedBy = d.SetpointW, d.MaxA, d.Reason, d.LimitedBy
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
				f.ObservedAtMs = measurement.Seen().UnixMilli()
			}
			if fresh && powerKnown && now.Sub(measurement.Seen()) <= telemetry.ControlPowerMaxAge(rd.Data) {
				f.ActualW = watts(measurement.PowerW)
			}
			if fresh && kind == telemetry.DerBattery && rd.SoC != nil &&
				!rd.SoCUpdatedAt.After(now) && now.Sub(rd.SoCUpdatedAt) <= telemetry.BatterySoCMaxAge && *rd.SoC >= 0 && *rd.SoC <= 1 {
				f.BatterySoC = watts(*rd.SoC)
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
			if full[rd.Driver] && kind == telemetry.DerBattery && f.BatterySoC != nil {
				coreReason = "battery_full"
				f.ChargeResumeSoC = watts(control.BatteryChargeResumeSoC)
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
			// Evidence describes measured power, not target fulfilment. A device
			// delivering 4.4 kW against a 5 kW command can still be site-confirmed.
			if commanded && cmd.Result == "accepted" && fresh && f.ActualW != nil && f.SentW != nil && cmd.HasMeasuredPower(now) {
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
				after, at := s.deps.Tel.ControlWindows(from, now), now
				if cmd.StepAfter != nil {
					// A completed step keeps the verdict its own window supported.
					after, at = cmd.StepAfter, cmd.StepAfterAt
				}
				comparison := s.controlResponse(cmd, meter, after, at)
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
					f.ConfirmedAtMs = comparison.AfterAt.UnixMilli()
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
				f.Reason = "readings_lost"
			}
			f.ReadingsFresh = fresh
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
	for i := range out {
		setControlStatus(&out[i])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Driver == out[j].Driver {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Driver < out[j].Driver
	})
	return out
}

// setControlStatus answers "Are we in control?" from the classified reason.
// Following needs measured readings; a driver's acknowledgement alone waits.
// Info needs no action, warning asks the owner to look and alarm means FTW
// cannot see or steer a device it is meant to control.
func setControlStatus(f *ControlFeedback) {
	status, severity := "waiting", "info"
	switch f.Reason {
	case "power_observed", "solar_below_ceiling", "idle", "plan", "manual_hold", "pv_surplus",
		"vehicle_complete", "vehicle_limit_completion", "vehicle_not_requesting":
		status = "following"
	case "battery_full", "battery_nearly_full", "battery_nearly_empty", "core_limit",
		"fuse_limit", "fuse_cooldown", "charger_limit", "load_balancer_limit":
		status = "limited"
	case "device_limit", "offered_current_lower":
		status, severity = "limited", "warning"
	case "setpoint_changed", "power_below_target", "power_above_target", "power_wrong_direction",
		"no_power_response", "power_while_idle":
		status, severity = "not_following", "warning"
	case "device_fault":
		status, severity = "not_following", "alarm"
	case "telemetry_stale", "command_failed", "command_unconfirmed", "response_unknown":
		status, severity = "no_contact", "warning"
	case "readings_lost", "default_failed":
		status, severity = "no_contact", "alarm"
	case "observe_only", "disabled", "device_control":
		status = "not_controlled"
	case "site_meter_stale", "site_phase_currents_stale":
		status, severity = "not_controlled", "warning"
	}
	if status == "following" && (f.VerificationTier == nil || *f.VerificationTier < 1) {
		status = "waiting"
	}
	f.Status, f.Severity = status, severity
	f.Evidence = "none"
	if f.VerificationTier != nil {
		f.Evidence = []string{"accepted", "measured", "confirmed"}[min(max(*f.VerificationTier, 0), 2)]
	}
}

// An acknowledgement is not an alarm during the normal response wait. A
// function that previously supplied measured proof must recover that proof.
// This state comes from telemetry, not from whether a browser was open.
func controlVerificationLost(f ControlFeedback, cmd telemetry.CommandEvidence, now time.Time) bool {
	// Failed or released calls have their own status. They do not by themselves
	// mean that a previously measured device stopped supplying power readings.
	if cmd.Result != "accepted" || cmd.LastMeasuredAt.IsZero() || f.VerificationTier != nil && *f.VerificationTier >= 1 {
		return false
	}
	switch f.Reason {
	case "observe_only", "disabled", "device_control", "not_connected", "idle", "no_command":
		return false
	}
	if f.Reason == "telemetry_stale" {
		return true
	}
	grace := 30 * time.Second
	if f.Kind == "ev" {
		grace = 2 * time.Minute
	}
	return !cmd.Since.IsZero() && now.Sub(cmd.Since) >= grace && now.Sub(cmd.LastMeasuredAt) >= grace
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
	// This is a known Core stop, not a BMS diagnosis or new measurement proof.
	// Continued power, changed setpoints, stale readings and faults keep their
	// own verdicts. A full battery is still allowed to discharge.
	if coreReason == "battery_full" && cmd.Result == "accepted" && f.SentW != nil && math.Abs(*f.SentW) < 1 &&
		f.ActualW != nil && math.Abs(*f.ActualW) <= telemetry.ControlToleranceW(0) {
		set("limited", "battery_full", "info")
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
	// The gap is measured against every command that could still be in force,
	// so a target retuned each tick does not read as a missed response.
	gap := math.Abs(*f.SentW - *f.ActualW)
	if f.Kind == "pv" {
		gap = math.Max(0, math.Abs(*f.ActualW)-math.Abs(*f.SentW))
	}
	if cmd.LastGapW != nil {
		gap = *cmd.LastGapW
	}
	if !cmd.PowerMismatchSince.IsZero() && now.Sub(cmd.PowerMismatchSince) >= grace && gap > telemetry.ControlToleranceW(*f.SentW) {
		if reason := batteryChargeLevelLimit(f); reason != "" {
			set("limited", reason, "info")
			return
		}
		// The charger names its load balancer only while it holds the car
		// back. More power than asked still warns.
		if f.Kind == "ev" && f.LimitedBy == "load_balancer" && *f.SentW > 100 && *f.ActualW > -100 && *f.ActualW < *f.SentW {
			set("limited", "load_balancer_limit", "info")
			return
		}
		reason := "power_above_target"
		switch {
		case math.Abs(*f.SentW) < 100:
			reason = "power_while_idle"
		case math.Abs(*f.ActualW) < 100:
			reason = "no_power_response"
		case f.Kind != "pv" && (*f.SentW < 0) != (*f.ActualW < 0):
			reason = "power_wrong_direction"
		case math.Abs(*f.ActualW) < math.Abs(*f.SentW):
			reason = "power_below_target"
		}
		set("warning", reason, "warning")
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
	if f.Kind == "pv" && math.Abs(*f.ActualW) < math.Abs(*f.SentW)-telemetry.ControlToleranceW(*f.SentW) {
		set("unknown", "solar_below_ceiling", "info")
		return
	}
	if f.Response != "device_reported" && f.Response != "site_confirmed" || cmd.PowerMatchSince.IsZero() || cmd.LastObservation.Sub(cmd.PowerMatchSince) < 10*time.Second {
		set("waiting", "waiting_response", "info")
		return
	}
	set("following", "power_observed", "info")
}

// A battery accepts less charge as it fills and less discharge as it empties.
// Its own fresh SoC explains that shortfall; wrong direction still warns.
func batteryChargeLevelLimit(f *ControlFeedback) string {
	if f.Kind != "battery" || f.BatterySoC == nil || f.SentW == nil || f.ActualW == nil {
		return ""
	}
	soc, sent, actual := *f.BatterySoC, *f.SentW, *f.ActualW
	tolerance := telemetry.ControlToleranceW(sent)
	switch {
	case sent > tolerance && soc >= 0.9 && actual > -tolerance && actual < sent:
		return "battery_nearly_full"
	case sent < -tolerance && soc <= 0.1 && actual < tolerance && actual > sent:
		return "battery_nearly_empty"
	}
	return ""
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
