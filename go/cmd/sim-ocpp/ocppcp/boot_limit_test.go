package ocppcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startFTW runs FTW's Central System with id named by a loadpoint.
func startFTW(t *testing.T, id string) (*ocpp.Server, DialOpts) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		cfg := &ocpp.Config{
			Enabled: true, Bind: "127.0.0.1", HeartbeatIntervalS: 60,
			Port: freePort(t), PortV201: freePort(t),
			ApprovedIDs: []string{id}, ManagedIDs: []string{id},
		}
		srv, err := ocpp.Start(context.Background(), cfg, telemetry.NewStore())
		if err != nil && attempt < 5 {
			continue // another test took the port
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Stop)
		return srv, DialOpts{
			URL16:  fmt.Sprintf("ws://127.0.0.1:%d", cfg.Port),
			URL201: fmt.Sprintf("ws://127.0.0.1:%d", cfg.PortV201),
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Seed 207534: the charger booted at 5.0 s, a car plugged in at 7.3 s and
// drew the charger's 16 A rating from 10 s until FTW's first 0 A landed at
// 13.5 s, with 18.9 A on a 16 A fuse. With the start limit, the charger holds
// 0 A from boot, and FTW's order still sets the rate afterwards.
func TestChargerHoldsZeroAmpsFromBootUntilFTWOrder(t *testing.T) {
	for _, modelID := range []string{"easee-charge-up", "charge-amps-aura", "defa-power"} {
		t.Run(modelID, func(t *testing.T) {
			m, ok := Lookup(modelID)
			if !ok {
				t.Fatalf("no model %s", modelID)
			}
			sim := New(m)
			srv, opts := startFTW(t, sim.DialID())
			waitFor(t, "the charger to connect", func() bool { return sim.Dial(opts) == nil })
			t.Cleanup(sim.Close)
			if got := sim.LimitA(); got != m.MaxAmps() {
				t.Fatalf("unsteered limit %v A, want the rating %v A", got, m.MaxAmps())
			}

			if err := sim.Boot(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the 0 A start limit", func() bool { return sim.LimitA() == 0 })

			if err := sim.Plug(); err != nil {
				t.Fatal(err)
			}
			sim.Tick(time.Second)
			if w := sim.PowerW(); w != 0 {
				t.Fatalf("car drew %v W before FTW's first order, want 0", w)
			}

			order, _ := json.Marshal(map[string]any{
				"action": "ev_set_current", "power_w": 6900.0, "voltage": 230.0, "site_phases": 3,
			})
			if err := srv.Command(context.Background(), sim.DialID(), order); err != nil {
				t.Fatalf("order: %v", err)
			}
			sim.Tick(time.Second)
			if got := sim.LimitA(); got != 10 {
				t.Fatalf("effective limit %v A after FTW's order, want 10 A", got)
			}
			if w := sim.PowerW(); w != 6900 {
				t.Fatalf("car draws %v W after FTW's order, want 6900", w)
			}
		})
	}
}
