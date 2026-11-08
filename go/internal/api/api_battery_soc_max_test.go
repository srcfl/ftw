package api

import (
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestSoCMaxFeedbackUsesTheDispatchResumeLevel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		soc := .9
		tel.Update("battery", telemetry.DerBattery, 0, &soc, nil)
		tel.RecordDriverSuccess("battery")
		tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
		tel.RecordDriverSuccess("meter")
		st := control.NewState(0, 50, "meter")
		st.Mode = control.ModeCharge
		st.DriverLimits = map[string]control.PowerLimits{"battery": {SoCMax: .9}}
		got := control.ComputeDispatch(tel, st, map[string]float64{"battery": 15000}, 13800)
		if len(got) != 1 || got[0].TargetW != 0 {
			t.Fatalf("charge not stopped: %+v", got)
		}
		request, _ := json.Marshal(map[string]any{"action": "battery", "power_w": 0})
		cmd := tel.BeginCommand("battery", request, time.Now())
		tel.CompleteCommand(cmd, "accepted")
		for range 16 {
			tel.Update("battery", telemetry.DerBattery, 0, &soc, []byte(`{"setpoint_w":0}`))
			time.Sleep(3 * time.Second)
		}
		srv := New(&Deps{Tel: tel, Ctrl: st, CtrlMu: &sync.Mutex{}})
		f := srv.controlFeedback(time.Now())[0]
		if f.Status != "limited" || f.Reason != "battery_full" || f.Severity != "info" || f.ChargeResumeSoC == nil || *f.ChargeResumeSoC != .89 {
			t.Fatalf("wrong explanation of the 90%% stop: %+v", f)
		}
	})
}
