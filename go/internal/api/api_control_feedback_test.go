package api

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"sync"
	"testing"
	"time"
)

func TestControlFeedbackReasons(t *testing.T) {
	now := time.Now()
	connected := true
	for _, tc := range []struct {
		name, kind, result, core, want             string
		fresh                                      bool
		sent, actual, readback, requestedA, limitA float64
		readMismatch, powerMismatch                bool
	}{
		{"driver accepted but no measured effect", "battery", "accepted", "", "power_differs", true, 1000, 0, 1000, 0, 0, false, true},
		{"Pixii overwritten while zero requested", "battery", "accepted", "", "setpoint_changed", true, 0, -1500, -1600, 0, 0, true, true},
		{"8 amp charger limit while charging", "ev", "accepted", "", "device_limit", true, 11040, 5500, 11040, 16, 8, false, true},
		{"fuse cap before dispatch", "ev", "accepted", "fuse_limit", "fuse_limit", true, 5000, 5000, 5000, 16, 0, false, false},
		{"driver refuses", "battery", "failed", "", "command_failed", true, 1000, 0, 0, 0, 0, false, false},
		{"unknown call outcome", "battery", "unconfirmed", "", "command_unconfirmed", true, 1000, 0, 0, 0, 0, false, false},
		{"default failed", "battery", "default_failed", "", "default_failed", true, 1000, 0, 0, 0, 0, false, false},
		{"device now autonomous", "battery", "released", "", "device_control", true, 1000, 0, 0, 0, 0, false, false},
		{"stale power cannot verify effect", "battery", "accepted", "", "telemetry_stale", false, 1000, 1000, 1000, 0, 0, false, false},
		{"device follows command", "battery", "accepted", "", "power_observed", true, 1000, 1000, 1000, 0, 0, false, false},
		{"Core clamp stays visible when followed", "battery", "accepted", "core_limit", "core_limit", true, 1000, 1000, 1000, 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := telemetry.CommandEvidence{Result: tc.result, Since: now.Add(-3 * time.Minute)}
			if tc.readMismatch {
				c.ReadbackMismatchSince = now.Add(-time.Minute)
			}
			if tc.powerMismatch {
				c.PowerMismatchSince = now.Add(-3 * time.Minute)
			}
			f := ControlFeedback{Kind: tc.kind, SentW: watts(tc.sent), ActualW: watts(tc.actual), ReadbackW: watts(tc.readback), ObservedAtMs: now.UnixMilli(), Response: "device_reported"}
			if tc.requestedA > 0 {
				f.RequestedA = watts(tc.requestedA)
			}
			if tc.limitA > 0 {
				f.DeviceLimitA = watts(tc.limitA)
			}
			classifyControlFeedback(&f, c, true, tc.fresh, tc.core, &connected, now)
			if f.Reason != tc.want {
				t.Fatalf("got %+v, want %s", f, tc.want)
			}
			if !tc.fresh && (f.ActualW != nil || f.ReadbackW != nil) {
				t.Fatal("stale numbers kept as current")
			}
		})
	}
}

func TestControlFeedbackEveryModeUsesEvidence(t *testing.T) {
	for _, mode := range control.AllModes() {
		t.Run(string(mode), func(t *testing.T) {
			tel := telemetry.NewStore()
			now := time.Now()
			c := tel.BeginCommand("battery", []byte(`{"action":"battery","power_w":1000}`), now.Add(-time.Minute))
			tel.CompleteCommand(c, "failed")
			tel.Update("battery", telemetry.DerBattery, 0, nil, []byte(`{}`))
			ctrl := &control.State{Mode: mode}
			srv := New(&Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{}, Cfg: &config.Config{}, CfgMu: &sync.RWMutex{}})
			feedback := srv.controlFeedback(time.Now())
			if len(feedback) != 1 || feedback[0].Reason != "command_failed" || feedback[0].VerificationTier != nil {
				t.Fatalf("mode %s: %+v", mode, feedback)
			}
		})
	}
}

func TestControlFeedbackRequiresMeasuredPowerNotSetpoint(t *testing.T) {
	tel := telemetry.NewStore()
	now := time.Now()
	c := tel.BeginCommand("battery", []byte(`{"action":"battery","power_w":1000}`), now)
	tel.CompleteCommand(c, "accepted")
	tel.Update("battery", telemetry.DerBattery, 0, nil, []byte(`{"setpoint_w":1000}`))
	srv := New(&Deps{Tel: tel})
	f := srv.controlFeedback(time.Now())[0]
	if f.VerificationTier == nil || *f.VerificationTier != 0 {
		t.Fatalf("setpoint echo reached measured tier: %+v", f)
	}
	tel.EndCommandControl("battery", false)
	f = srv.controlFeedback(time.Now())[0]
	if f.VerificationTier != nil || f.Reason != "device_control" {
		t.Fatalf("old command survived default: %+v", f)
	}
}

func TestControlFeedbackDoesNotVerifyOneCachedSourceSample(t *testing.T) {
	tel := telemetry.NewStore()
	now := time.Now()
	c := tel.BeginCommand("battery", []byte(`{"action":"battery","power_w":1000}`), now.Add(-time.Minute))
	tel.CompleteCommand(c, "accepted")
	data := []byte(`{"power_observed_at":"` + now.Add(-10*time.Second).Format(time.RFC3339Nano) + `"}`)
	tel.Update("battery", telemetry.DerBattery, 1000, nil, data)
	tel.Update("battery", telemetry.DerBattery, 1000, nil, data)
	srv := New(&Deps{Tel: tel})
	f := srv.controlFeedback(now)[0]
	if f.VerificationTier == nil || *f.VerificationTier != 0 {
		t.Fatalf("one source sample reached measured tier: %+v", f)
	}
}

func TestControlTiersBelongToCommandedFunctions(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("solar", telemetry.DerPV, -3000, nil, []byte(`{}`))
	tel.Update("monitored-battery", telemetry.DerBattery, 0, nil, []byte(`{}`))
	srv := New(&Deps{Tel: tel})
	if got := srv.controlFeedback(time.Now()); len(got) != 0 {
		t.Fatalf("measurement sources received control verdicts: %+v", got)
	}
	cmd := tel.BeginCommand("solar", []byte(`{"action":"curtail","power_w":-1000}`), time.Now())
	tel.CompleteCommand(cmd, "accepted")
	got := srv.controlFeedback(time.Now())
	if len(got) != 1 || got[0].Driver != "solar" || got[0].VerificationTier == nil || *got[0].VerificationTier != 0 {
		t.Fatalf("curtailment missing command verdict: %+v", got)
	}
}

func TestControlVerificationLossAlarm(t *testing.T) {
	now := time.Now()
	one, zero := 1, 0
	for _, tc := range []struct {
		name, reason, kind   string
		tier                 *int
		verified, commandAge time.Duration
		want                 bool
	}{
		{"startup", "waiting_response", "battery", &zero, 0, time.Minute, false},
		{"new command settling", "waiting_response", "battery", &zero, time.Minute, 5 * time.Second, false},
		{"lost measured response", "waiting_response", "battery", &zero, time.Minute, time.Minute, true},
		{"device offline", "telemetry_stale", "battery", &zero, 20 * time.Second, time.Minute, true},
		{"EV still settling", "waiting_response", "ev", &zero, time.Minute, time.Minute, false},
		{"EV lost response", "response_unknown", "ev", &zero, 3 * time.Minute, 3 * time.Minute, true},
		{"unplugged", "not_connected", "ev", &zero, 3 * time.Minute, 3 * time.Minute, false},
		{"released", "device_control", "battery", nil, time.Minute, time.Minute, false},
		{"device recovers", "power_observed", "battery", &one, time.Minute, time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := telemetry.CommandEvidence{Result: "accepted", Since: now.Add(-tc.commandAge)}
			if tc.verified > 0 {
				cmd.LastVerifiedAt = now.Add(-tc.verified)
			}
			f := ControlFeedback{Reason: tc.reason, Kind: tc.kind, VerificationTier: tc.tier}
			if got := controlVerificationLost(f, cmd, now); got != tc.want {
				t.Fatalf("alarm=%v want %v", got, tc.want)
			}
		})
	}
}
