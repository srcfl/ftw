package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestOCPPReconnectDoesNotUnplugOrResumeOldStart(t *testing.T) {
	for _, power := range []float64{0, 4140} {
		for _, skipUnknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("power=%v/fast=%v", power, skipUnknown), func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := listener.Addr().(*net.TCPAddr).Port
				listener.Close()
				tel := telemetry.NewStore()
				srv, err := ocpp.Start(context.Background(), &ocpp.Config{Enabled: true, Bind: "127.0.0.1", Port: port, ApprovedIDs: []string{"garage"}}, tel)
				if err != nil {
					t.Fatal(err)
				}
				defer srv.Stop()
				await := func(fn func() bool) {
					t.Helper()
					deadline := time.Now().Add(2 * time.Second)
					for !fn() {
						if time.Now().After(deadline) {
							t.Fatal("OCPP state did not settle")
						}
						time.Sleep(time.Millisecond)
					}
				}
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				await(func() bool {
					c, e := net.DialTimeout("tcp", addr, 10*time.Millisecond)
					if e != nil {
						return false
					}
					c.Close()
					return true
				})
				connect := func() ocpp16.ChargePoint {
					t.Helper()
					cp := ocpp16.NewChargePoint("garage", nil, nil)
					if e := cp.Start("ws://" + addr); e != nil {
						t.Fatal(e)
					}
					return cp
				}
				boot := func(cp ocpp16.ChargePoint, serial string) {
					t.Helper()
					_, e := cp.BootNotification("Home", "Easee", func(r *core.BootNotificationRequest) { r.ChargePointSerialNumber = serial })
					if e != nil {
						t.Fatal(e)
					}
				}
				status := func(cp ocpp16.ChargePoint, connected bool) {
					t.Helper()
					s := core.ChargePointStatusAvailable
					if connected {
						s = core.ChargePointStatusSuspendedEVSE
					}
					if _, e := cp.StatusNotification(1, core.NoError, s); e != nil {
						t.Fatal(e)
					}
				}
				cp := connect()
				boot(cp, "A")
				status(cp, true)
				manager := loadpoint.NewManager()
				manager.Load([]loadpoint.Config{{ID: "garage", DriverName: "garage", MinChargeW: 1380, MaxChargeW: 11040, VehicleCapacityWh: 60000}})
				now := time.Now()
				manager.SetNowFn(func() time.Time { return now })
				bus := events.NewBus()
				manager.SetBus(bus)
				connectedEvents := 0
				bus.Subscribe(events.KindChargingConnected, func(events.Event) { connectedEvents++ })
				bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{"garage": {Status: telemetry.StatusOk, LastSuccess: &now}}, Now: now})
				commands := []float64{}
				ctrl := loadpoint.NewController(manager, func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false }, func(name string) (loadpoint.EVSample, bool) {
					return currentEVSample(tel.Get(name, telemetry.DerEV), tel.DriverHealth(name), time.Minute, time.Now(), srv.Handler().IsOnline(name), currentOCPPDeviceID(srv.Handler(), name))
				}, func(_ context.Context, _ string, p []byte) error {
					var d struct {
						Power float64 `json:"power_w"`
					}
					if e := json.Unmarshal(p, &d); e != nil {
						return e
					}
					commands = append(commands, d.Power)
					return nil
				})
				ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
				tick := func() { ctrl.Tick(context.Background(), now); now = now.Add(3 * time.Second) }
				tick()
				manager.SetCurrentSoC("garage", .12)
				ctrl.SetManualHold("garage", loadpoint.ManualHold{Persistent: true, PowerW: power})
				tick()
				before, _ := manager.State("garage")
				commandCount := len(commands)
				cp.Stop()
				await(func() bool {
					r := tel.Get("garage", telemetry.DerEV)
					var d struct {
						Unknown bool `json:"connection_unknown"`
					}
					return r != nil && json.Unmarshal(r.Data, &d) == nil && d.Unknown && !srv.Handler().IsOnline("garage")
				})
				checkUnknown := func() {
					t.Helper()
					tick()
					st, _ := manager.State("garage")
					hold, ok := ctrl.GetManualHold("garage", now)
					if !ok || hold.PowerW != 0 || !st.PluggedIn || st.ManualRestoreUnconfirmed != (power > 0) {
						t.Fatalf("unknown socket lost safety: hold=%+v state=%+v", hold, st)
					}
					if len(commands) != commandCount {
						t.Fatal("unknown cable dispatched a command")
					}
					if st.UpdatedAtMs != before.UpdatedAtMs {
						t.Fatal("socket event refreshed physical observation")
					}
					if st.SoCSource != "assumed" {
						t.Fatalf("kept confirmed SoC after identity loss: %s", st.SoCSource)
					}
					endpoint := api.New(&api.Deps{Loadpoints: manager, LoadpointCtrl: ctrl, Tel: tel})
					rr := httptest.NewRecorder()
					endpoint.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
					var body struct {
						Loadpoints []loadpoint.State `json:"loadpoints"`
					}
					if e := json.Unmarshal(rr.Body.Bytes(), &body); e != nil {
						t.Fatal(e)
					}
					if len(body.Loadpoints) != 1 || body.Loadpoints[0].Charger.Available || body.Loadpoints[0].Manual.State != loadpoint.ManualUnavailable {
						t.Fatalf("unknown socket became pause acknowledgement: %s", rr.Body.String())
					}
				}
				if !skipUnknown {
					checkUnknown()
				}
				cp = connect()
				defer cp.Stop()
				serial := "B"
				if skipUnknown {
					serial = "A"
				}
				boot(cp, serial)
				if !skipUnknown {
					checkUnknown()
				}
				status(cp, true)
				tick()
				hold, ok := ctrl.GetManualHold("garage", now)
				if !ok || hold.PowerW != 0 || commands[len(commands)-1] != 0 {
					t.Fatalf("status revived an old Start: %+v %v", hold, commands)
				}
				if st, _ := manager.State("garage"); st.ManualRestoreUnconfirmed != (power > 0) {
					t.Fatal("status changed required confirmation")
				}
				if connectedEvents != 0 {
					t.Fatalf("socket reconnect invented %d plug notifications", connectedEvents)
				}
				ctrl.SetManualHold("garage", loadpoint.ManualHold{Persistent: true, PowerW: 4140})
				tick()
				if commands[len(commands)-1] != 4140 {
					t.Fatal("fresh explicit Start did not work")
				}
				status(cp, false)
				tick()
				if _, ok := ctrl.GetManualHold("garage", now); ok {
					t.Fatal("physical unplug did not clear hold")
				}
				if st, _ := manager.State("garage"); st.PluggedIn {
					t.Fatal("physical unplug not recorded")
				}
				status(cp, true)
				tick()
				if connectedEvents != 1 {
					t.Fatalf("real plug edge notifications=%d", connectedEvents)
				}
			})
		}
	}
}
