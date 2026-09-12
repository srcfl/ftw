package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/srcfl/ftw/go/cmd/sim-ocpp/ocppcp"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func evifyWaitBound(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener never bound on %d", port)
}

func evifyStartCS(t *testing.T, approved []string) (url16, url201 string, srv *ocpp.Server) {
	t.Helper()
	p16, p201 := freePort(t), freePort(t)
	cfg := &ocpp.Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               p16,
		PortV201:           p201,
		HeartbeatIntervalS: 60,
		ApprovedIDs:        approved,
	}
	s, err := ocpp.Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	evifyWaitBound(t, p16)
	evifyWaitBound(t, p201)
	return fmt.Sprintf("ws://127.0.0.1:%d", p16), fmt.Sprintf("ws://127.0.0.1:%d", p201), s
}

func evifyPayload(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func evifyWaitOnline(t *testing.T, srv *ocpp.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Handler().IsOnline(id) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never came online", id)
}

func evifyWaitSteerable(t *testing.T, srv *ocpp.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := srv.Handler().Snapshot()[id]
		if v.Steerable != nil && *v.Steerable {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never reported SmartCharging: %+v", id, srv.Handler().Snapshot()[id])
}

// evifyWaitPower reports until FTW telemetry shows want watts.
//
// ocpp-go serializes MeterValues timestamps as RFC3339 (second resolution).
// FTW's recordPower treats a sample whose measured Unix milli is not
// strictly greater than the last accepted one as a replay, so a pause
// that lands in the same second as the previous meter value never shows
// up. Retrying after the next UTC second is what a 1 Hz charge point
// already does on the wire.
func evifyWaitPower(t *testing.T, tel *telemetry.Store, sim *ocppcp.Sim, want float64) {
	t.Helper()
	id := sim.DialID()
	deadline := time.Now().Add(5 * time.Second)
	var last float64
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if attempt > 0 {
			next := time.Now().UTC().Truncate(time.Second).Add(time.Second)
			time.Sleep(time.Until(next) + 15*time.Millisecond)
		}
		sim.Tick(0)
		if err := sim.Report(); err != nil {
			t.Fatalf("meter %s: %v", id, err)
		}
		r := tel.Get(id, telemetry.DerEV)
		if r != nil {
			last = r.RawW
			if evifyAbs(r.RawW-want) < 1 {
				return
			}
		}
	}
	t.Fatalf("%s power=%v, want %v (sim=%.0f W limit=%.1f A)", id, last, want, sim.PowerW(), sim.LimitA())
}

func evifyAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func evifyDialAll(t *testing.T, url16, url201 string) []*ocppcp.Sim {
	t.Helper()
	models := ocppcp.OCPPModels()
	sims := make([]*ocppcp.Sim, 0, len(models))
	for _, m := range models {
		sim := ocppcp.New(m)
		if err := sim.Dial(ocppcp.DialOpts{URL16: url16, URL201: url201}); err != nil {
			t.Fatalf("dial %s: %v", m.ID, err)
		}
		t.Cleanup(sim.Close)
		if err := sim.Boot(); err != nil {
			t.Fatalf("boot %s: %v", m.ID, err)
		}
		sims = append(sims, sim)
	}
	return sims
}

// TestEvifyOCPPInventoryE2E connects every OCPP charger Evify currently
// stocks to one FTW Central System, adopts them, plugs a car in, steers
// current, and pauses. Tesla is excluded because it has no OCPP.
func TestEvifyOCPPInventoryE2E(t *testing.T) {
	if os.Getenv("FTW_E2E") != "1" {
		t.Skip("set FTW_E2E=1 to run the OCPP integration test")
	}
	models := ocppcp.OCPPModels()
	approved := make([]string, 0, len(models))
	for _, m := range models {
		approved = append(approved, m.DialID())
	}

	tel := telemetry.NewStore()
	p16, p201 := freePort(t), freePort(t)
	srv, err := ocpp.Start(context.Background(), &ocpp.Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               p16,
		PortV201:           p201,
		HeartbeatIntervalS: 60,
		ApprovedIDs:        approved,
	}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	evifyWaitBound(t, p16)
	evifyWaitBound(t, p201)
	url16 := fmt.Sprintf("ws://127.0.0.1:%d", p16)
	url201 := fmt.Sprintf("ws://127.0.0.1:%d", p201)

	sims := evifyDialAll(t, url16, url201)

	for _, sim := range sims {
		id := sim.DialID()
		evifyWaitOnline(t, srv, id)
		evifyWaitSteerable(t, srv, id)
		view := srv.Handler().Snapshot()[id]
		if view.Pending {
			t.Errorf("%s stayed pending after adoption", id)
		}
		if view.Vendor != sim.Model.Vendor {
			t.Errorf("%s vendor=%q, want %q", id, view.Vendor, sim.Model.Vendor)
		}
		if view.Serial != sim.Model.Serial {
			t.Errorf("%s serial=%q, want %q", id, view.Serial, sim.Model.Serial)
		}
		if sim.Model.Protocol == ocppcp.ProtocolOCPP201 && view.Version != string(ocpp.Version201) {
			t.Errorf("%s version=%q, want 2.0.1", id, view.Version)
		}
		if sim.Model.Protocol == ocppcp.ProtocolOCPP16 && view.Version != string(ocpp.Version16) {
			t.Errorf("%s version=%q, want 1.6", id, view.Version)
		}
	}

	const setW = 6900.0 // 10 A × 230 V × 3 — under Halo's 11 kW ceiling
	cmd := evifyPayload(t, map[string]any{
		"action": "ev_set_current", "power_w": setW, "voltage": ocppcp.SiteVoltage, "site_phases": 3,
	})
	pause := evifyPayload(t, map[string]any{"action": "ev_pause"})

	for _, sim := range sims {
		sim := sim
		id := sim.DialID()
		t.Run(sim.Model.ID, func(t *testing.T) {
			if err := sim.Plug(); err != nil {
				t.Fatalf("plug: %v", err)
			}
			if err := srv.Command(context.Background(), id, cmd); err != nil {
				t.Fatalf("set current: %v", err)
			}
			if got, want := sim.LimitA(), 10.0; evifyAbs(got-want) > 0.05 {
				t.Fatalf("limit=%v A after set, want %v", got, want)
			}
			evifyWaitPower(t, tel, sim, setW)

			if err := srv.Command(context.Background(), id, pause); err != nil {
				t.Fatalf("pause: %v", err)
			}
			if got := sim.LimitA(); got != 0 {
				t.Fatalf("limit=%v A after pause, want 0", got)
			}
			evifyWaitPower(t, tel, sim, 0)
		})
	}

	var aura *ocppcp.Sim
	var zap *ocppcp.Sim
	for _, sim := range sims {
		switch sim.Model.ID {
		case "charge-amps-aura":
			aura = sim
		case "zaptec-go":
			zap = sim
		}
	}
	if aura == nil || zap == nil {
		t.Fatal("missing Aura or Zaptec Go in the connected set")
	}

	var refused0, retried1 bool
	for _, a := range aura.Attempts() {
		if a.ConnectorID == 0 && a.Status == "Rejected" {
			refused0 = true
		}
		if refused0 && a.ConnectorID == 1 && a.Status == "Accepted" && a.Applied {
			retried1 = true
			break
		}
	}
	if !refused0 || !retried1 {
		t.Errorf("Aura should refuse connector 0 then accept 1, got %+v", aura.Attempts())
	}

	if zap.DialID() != zap.Model.Serial {
		t.Errorf("Zaptec dialled as %s, want serial %s", zap.DialID(), zap.Model.Serial)
	}
	if _, ok := srv.Handler().Snapshot()[zap.Model.Serial]; !ok {
		t.Errorf("FTW keyed Zaptec on something other than the serial: %v", srv.Handler().Snapshot())
	}
}

func TestPendingEvifyChargerIsQuarantined(t *testing.T) {
	if os.Getenv("FTW_E2E") != "1" {
		t.Skip("set FTW_E2E=1 to run the OCPP integration test")
	}
	m, _ := ocppcp.Lookup("easee-charge-up")
	url16, _, srv := evifyStartCS(t, nil)
	sim := ocppcp.New(m)
	if err := sim.Dial(ocppcp.DialOpts{URL16: url16}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	if err := sim.Boot(); err != nil {
		t.Fatal(err)
	}
	evifyWaitOnline(t, srv, m.DialID())
	if err := sim.Plug(); err != nil {
		t.Fatal(err)
	}
	sim.Tick(time.Second)
	if err := sim.Report(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v := srv.Handler().Snapshot()[m.DialID()]
		if v.PowerW > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	v := srv.Handler().Snapshot()[m.DialID()]
	if !v.Pending {
		t.Fatalf("unadopted charger must stay pending: %+v", v)
	}
	if v.Vendor != m.Vendor {
		t.Errorf("pending charger should still show vendor, got %+v", v)
	}
}
