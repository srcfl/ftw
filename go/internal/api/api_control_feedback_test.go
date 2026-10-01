package api

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"sync"
	"testing"
	"testing/synctest"
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
		{"driver accepted but no measured effect", "battery", "accepted", "", "no_power_response", true, 1000, 0, 1000, 0, 0, false, true},
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
			c := telemetry.CommandEvidence{Result: tc.result, Since: now.Add(-3 * time.Minute), PowerMatchSince: now.Add(-time.Minute), LastObservation: now}
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
				cmd.LastMeasuredAt = now.Add(-tc.verified)
			}
			f := ControlFeedback{Reason: tc.reason, Kind: tc.kind, VerificationTier: tc.tier}
			if got := controlVerificationLost(f, cmd, now); got != tc.want {
				t.Fatalf("alarm=%v want %v", got, tc.want)
			}
		})
	}
}

func TestConfiguredBatteryVisibleBeforeFirstCommand(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("battery", telemetry.DerBattery, -300, nil, []byte(`{}`))
	cfg := &config.Config{Drivers: []config.Driver{{Name: "battery", BatteryCapacityWh: 10000}}}
	srv := New(&Deps{Tel: tel, Cfg: cfg, CfgMu: &sync.RWMutex{}})
	got := srv.controlFeedback(time.Now())
	if len(got) != 1 || got[0].Reason != "no_command" || got[0].VerificationTier != nil || got[0].VerificationLost {
		t.Fatalf("pre-command battery needs a neutral status, got %+v", got)
	}
}

// Exercise telemetry-to-API with a fake clock. The site sees actual device
// watts, not requested watts; no UI poll supplies the proof window.
func TestControlEvidenceIsIndependentOfTargetFulfilment(t *testing.T) {
	for _, tc := range []struct {
		name, action, reason, site        string
		kind                              telemetry.DerType
		target, actual, before, extraLoad float64
		seconds, tier                     int
	}{
		{"battery discharge shortfall", "battery", "power_below_target", "confirmed", telemetry.DerBattery, -5000, -4400, 0, 0, 42, 2},
		{"battery charge shortfall", "battery", "power_below_target", "confirmed", telemetry.DerBattery, 5000, 4400, 0, 0, 42, 2},
		{"EV shortfall", "ev_set_current", "power_below_target", "confirmed", telemetry.DerEV, 11000, 5500, 0, 0, 132, 2},
		{"V2X shortfall", "v2x_set_power", "power_below_target", "confirmed", telemetry.DerV2X, -5000, -4400, 0, 0, 42, 2},
		{"shortfall with unexplained load", "battery", "power_below_target", "site_change_differs", telemetry.DerBattery, -5000, -4400, 0, 2000, 42, 1},
		{"already at lower output", "battery", "power_below_target", "no_clear_change", telemetry.DerBattery, -5000, -4400, -4400, 0, 42, 1},
		{"no response is measured but not followed", "battery", "no_power_response", "no_clear_change", telemetry.DerBattery, -5000, 0, 0, 0, 42, 1},
		{"opposite response is confirmed but wrong", "battery", "power_wrong_direction", "confirmed", telemetry.DerBattery, -5000, 4400, 0, 0, 42, 2},
		{"overshoot is confirmed but wrong", "battery", "power_above_target", "confirmed", telemetry.DerBattery, -5000, -6000, 0, 0, 42, 2},
		{"ignored idle is confirmed but wrong", "battery", "power_while_idle", "confirmed", telemetry.DerBattery, 0, -4400, 0, 0, 42, 2},
		{"PV below ceiling is not proven curtailment", "curtail", "solar_below_ceiling", "confirmed", telemetry.DerPV, -3000, -1000, -5000, 0, 42, 2},
		{"ordinary response wait", "battery", "waiting_response", "device_response_unconfirmed", telemetry.DerBattery, -5000, -4400, 0, 0, 6, 0},
		{"matching response", "battery", "power_observed", "confirmed", telemetry.DerBattery, -5000, -5000, 0, 0, 42, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := telemetry.NewStore()
				read := func(power, extra float64) {
					tel.Update("hybrid", tc.kind, power, nil, []byte(`{}`))
					tel.Update("hybrid", telemetry.DerMeter, 800+power+extra, nil, []byte(`{"power_origin":"external_meter"}`))
					tel.RecordDriverSuccess("hybrid")
				}
				for range 5 {
					read(tc.before, 0)
					time.Sleep(3 * time.Second)
				}
				request, _ := json.Marshal(map[string]any{"action": tc.action, "power_w": tc.target})
				c := tel.BeginCommand("hybrid", request, time.Now())
				tel.CompleteCommand(c, "accepted")
				for elapsed := 0; elapsed <= tc.seconds; elapsed += 3 {
					read(tc.actual, tc.extraLoad)
					time.Sleep(3 * time.Second)
				}
				srv := New(&Deps{Tel: tel, Ctrl: &control.State{SiteMeterDriver: "hybrid"}, CtrlMu: &sync.Mutex{}})
				f := srv.controlFeedback(time.Now())[0]
				if f.VerificationTier == nil || *f.VerificationTier != tc.tier || f.Reason != tc.reason || f.SiteConfirmation != tc.site || f.VerificationLost {
					t.Fatalf("got %+v; want tier %d, %s, %s with no lost-measurement alarm", f, tc.tier, tc.reason, tc.site)
				}
				if f.ActualW == nil || *f.ActualW != tc.actual || f.DeviceReason != "" {
					t.Fatalf("changed watts or invented a cause: %+v", f)
				}
				if tc.tier == 2 && (f.DeviceDeltaW == nil || math.Abs(*f.DeviceDeltaW-(tc.actual-tc.before)) > 1) {
					t.Fatalf("confirmed target instead of actual change: %+v", f)
				}
				if tc.reason == "power_below_target" && tc.tier == 2 {
					tel.Update("hybrid", tc.kind, 0, nil, []byte(`{"control_power_available":false}`))
					time.Sleep(3 * time.Minute)
					lost := srv.controlFeedback(time.Now())[0]
					if lost.VerificationTier == nil || *lost.VerificationTier != 0 || !lost.VerificationLost || lost.ActualW != nil {
						t.Fatalf("missing power kept proof: %+v", lost)
					}
					for range 6 {
						read(tc.target, 0)
						time.Sleep(3 * time.Second)
					}
					recovered := srv.controlFeedback(time.Now())[0]
					if recovered.VerificationTier == nil || *recovered.VerificationTier != 2 || recovered.Reason != "power_observed" || recovered.VerificationLost {
						t.Fatalf("fresh recovery failed: %+v", recovered)
					}
				}
			})
		})
	}
}

func TestFailedCallIsNotLostMeasurement(t *testing.T) {
	now := time.Now()
	for _, result := range []string{"failed", "unconfirmed", "default_failed", "pending", "released"} {
		cmd := telemetry.CommandEvidence{Result: result, Since: now.Add(-time.Minute), LastMeasuredAt: now.Add(-time.Minute)}
		if controlVerificationLost(ControlFeedback{Reason: "command_failed", ActualW: watts(-4400)}, cmd, now) {
			t.Fatalf("%s call was relabelled as lost measurements", result)
		}
	}
}

func TestFullBatteryFeedbackKeepsEvidenceAndFaultsSeparate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target, actual float64
		data, reason   string
		fault, stale   bool
	}{
		{name: "full stop", target: 0, actual: 0, data: `{"setpoint_w":0}`, reason: "battery_full"},
		{name: "stop ignored", target: 0, actual: 500, data: `{"setpoint_w":0}`, reason: "power_while_idle"},
		{name: "discharge ignored", target: -1000, actual: 0, data: `{"setpoint_w":-1000}`, reason: "no_power_response"},
		{name: "discharge allowed", target: -1000, actual: -1000, data: `{"setpoint_w":-1000}`, reason: "power_observed"},
		{name: "fault", target: 0, actual: 0, data: `{"setpoint_w":0}`, reason: "device_fault", fault: true},
		{name: "stale", target: 0, actual: 0, data: `{"setpoint_w":0}`, reason: "readings_lost", stale: true},
		{name: "setpoint changed", target: 0, actual: 0, data: `{"setpoint_w":1000}`, reason: "setpoint_changed"},
		{name: "power unknown", target: 0, actual: 0, data: `{"setpoint_w":0,"control_power_available":false}`, reason: "response_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := telemetry.NewStore()
				soc := 1.0
				tel.Update("battery", telemetry.DerBattery, 0, &soc, []byte(`{}`))
				tel.RecordDriverSuccess("battery")
				tel.Update("meter", telemetry.DerMeter, -1000, nil, []byte(`{}`))
				tel.RecordDriverSuccess("meter")
				st := control.NewState(0, 50, "meter")
				st.Mode = control.ModeCharge
				targets := control.ComputeDispatch(tel, st, map[string]float64{"battery": 9600}, 13800)
				if len(targets) != 1 || targets[0].TargetW != 0 {
					t.Fatalf("full battery not stopped: %+v", targets)
				}
				request, _ := json.Marshal(map[string]any{"action": "battery", "power_w": tc.target})
				c := tel.BeginCommand("battery", request, time.Now())
				tel.CompleteCommand(c, "accepted")
				for range 16 {
					tel.Update("battery", telemetry.DerBattery, tc.actual, &soc, []byte(tc.data))
					time.Sleep(3 * time.Second)
				}
				if tc.fault {
					tel.SetDriverDeviceFault("battery", true, "test fault")
				}
				if tc.stale {
					time.Sleep(2 * time.Minute)
				}
				srv := New(&Deps{Tel: tel, Ctrl: st, CtrlMu: &sync.Mutex{}})
				f := srv.controlFeedback(time.Now())[0]
				if f.Reason != tc.reason {
					t.Fatalf("got %+v want %s", f, tc.reason)
				}
				if tc.reason == "battery_full" {
					if f.Severity != "info" || f.VerificationTier == nil || *f.VerificationTier != 1 || f.ActualW == nil || *f.ActualW != 0 || f.BatterySoC == nil || *f.BatterySoC != 1 || f.ChargeResumeSoC == nil || *f.ChargeResumeSoC != .99 {
						t.Fatalf("full stop invented proof or lost context: %+v", f)
					}
				}
			})
		})
	}
}
