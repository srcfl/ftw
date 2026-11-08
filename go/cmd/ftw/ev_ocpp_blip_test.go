package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/ocpp/chargersocket"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A Wi-Fi blip drops the charger's socket while Charge now runs. The same
// charger comes back still plugged in, and no unplug was seen.
func TestChargeNowSurvivesOCPPSocketBlip(t *testing.T) {
	for _, tc := range []struct {
		name        string
		serial      string
		seeUnknown  bool // a control tick runs while the socket is down
		unplugAfter bool // the charger reports Available after it returns
		wantW       float64
	}{
		{name: "same charger", serial: "A", seeUnknown: true, wantW: 11040},
		{name: "same charger, blip between ticks", serial: "A", wantW: 11040},
		{name: "other charger", serial: "B", seeUnknown: true, wantW: 0},
		{name: "unplug reported after return", serial: "A", seeUnknown: true, unplugAfter: true, wantW: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			connect := func(serial string) ocpp16.ChargePoint {
				t.Helper()
				cp := chargersocket.ChargePoint16("garage", nil)
				if e := cp.Start("ws://" + addr); e != nil {
					t.Fatal(e)
				}
				if _, e := cp.BootNotification("Home", "Easee", func(r *core.BootNotificationRequest) { r.ChargePointSerialNumber = serial }); e != nil {
					t.Fatal(e)
				}
				return cp
			}
			status := func(cp ocpp16.ChargePoint, s core.ChargePointStatus) {
				t.Helper()
				if _, e := cp.StatusNotification(1, core.NoError, s); e != nil {
					t.Fatal(e)
				}
			}
			cp := connect("A")
			status(cp, core.ChargePointStatusCharging)
			manager := loadpoint.NewManager()
			manager.Load([]loadpoint.Config{{ID: "garage", DriverName: "garage", MinChargeW: 1380, MaxChargeW: 11040, VehicleCapacityWh: 60000}})
			now := time.Now()
			manager.SetNowFn(func() time.Time { return now })
			bus := events.NewBus()
			manager.SetBus(bus)
			plugEdges := 0
			bus.Subscribe(events.KindChargingConnected, func(events.Event) { plugEdges++ })
			var commands []float64
			ctrl := loadpoint.NewController(manager, func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false }, func(name string) (loadpoint.EVSample, bool) {
				return currentEVSample(tel.Get(name, telemetry.DerEV), tel.DriverHealth(name), time.Minute, time.Now(), srv.Handler().IsOnline(name), currentOCPPDeviceID(srv.Handler(), name))
			}, func(_ context.Context, _ string, p []byte) error {
				var d struct {
					Power  float64 `json:"power_w"`
					Action string  `json:"action"`
				}
				if e := json.Unmarshal(p, &d); e != nil {
					return e
				}
				if d.Action == "ev_set_current" {
					commands = append(commands, d.Power)
				}
				return nil
			})
			ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			tick := func() { ctrl.Tick(context.Background(), now); now = now.Add(3 * time.Second) }
			tick()
			ctrl.SetManualHold("garage", loadpoint.ManualHold{Persistent: true, PowerW: 11040})
			tick()
			if len(commands) == 0 || commands[len(commands)-1] != 11040 {
				t.Fatalf("Charge now did not start: %v", commands)
			}
			sent := len(commands)

			cp.Stop()
			await(func() bool { return !srv.Handler().IsOnline("garage") })
			if tc.seeUnknown {
				tick()
			}
			cp = connect(tc.serial)
			defer cp.Stop()
			status(cp, core.ChargePointStatusCharging)
			if tc.unplugAfter {
				tick()
				status(cp, core.ChargePointStatusAvailable)
			}
			tick()
			tick()
			if tc.unplugAfter {
				// The cable stays out for UnplugGrace.
				now = now.Add(loadpoint.UnplugGrace)
				tick()
			}

			after := commands[sent:]
			hold, held := ctrl.GetManualHold("garage", now)
			st, _ := manager.State("garage")
			t.Logf("after the blip: commands=%v hold=%+v held=%v unconfirmed=%v plugged=%v", after, hold, held, st.ManualRestoreUnconfirmed, st.PluggedIn)
			if !tc.unplugAfter && (!st.PluggedIn || plugEdges != 0) {
				t.Fatalf("the blip looked like an unplug: plugged=%v plug edges=%d", st.PluggedIn, plugEdges)
			}
			if tc.wantW > 0 {
				if len(after) == 0 {
					t.Fatal("no command after the blip")
				}
				for _, w := range after {
					if w != tc.wantW {
						t.Fatalf("the blip changed Charge now: commands after it %v, want only %v", after, tc.wantW)
					}
				}
				if !held || hold.PowerW != tc.wantW || st.ManualRestoreUnconfirmed {
					t.Fatalf("Charge now did not survive: hold=%+v held=%v unconfirmed=%v", hold, held, st.ManualRestoreUnconfirmed)
				}
				return
			}
			if tc.unplugAfter {
				// An unplug ends Charge now; an empty charger gets no command.
				if held || st.PluggedIn {
					t.Fatalf("unplug did not end Charge now: hold=%+v held=%v plugged=%v", hold, held, st.PluggedIn)
				}
				return
			}
			if !held || hold.PowerW != 0 || !st.ManualRestoreUnconfirmed || len(after) == 0 || after[len(after)-1] != 0 {
				t.Fatalf("old Charge now followed %s: commands=%v hold=%+v unconfirmed=%v", tc.name, after, hold, st.ManualRestoreUnconfirmed)
			}
		})
	}
}
