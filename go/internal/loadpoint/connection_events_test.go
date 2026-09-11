package loadpoint

import (
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestConnectionEventNeedsFreshEdgeAndSurvivesReload(t *testing.T) {
	m := NewManager()
	cfg := []Config{{ID: "garage", DriverName: "easee"}}
	m.Load(cfg)
	now := time.Unix(1700000000, 0)
	m.SetNowFn(func() time.Time { return now })
	b := events.NewBus()
	m.SetBus(b)
	count := 0
	b.Subscribe(events.KindChargingConnected, func(e events.Event) {
		count++
		if e.(events.ChargingConnected).LoadpointID != "garage" {
			t.Fatal(e)
		}
		m.State("garage")
	})
	health := func(status telemetry.DriverStatus) {
		b.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{"easee": {Status: status, LastSuccess: &now}}, Now: now})
	}
	tick := func(connected bool) { m.Observe("garage", connected, 0, 0, true); now = now.Add(3 * time.Second) }
	// Registered does not mean a charger has supplied its first reading.
	b.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{"easee": {Status: telemetry.StatusOk}}, Now: now})
	tick(false)
	health(telemetry.StatusOk)
	tick(true)
	tick(true)
	if count != 0 {
		t.Fatal("startup is not a new plug-in")
	}
	tick(false)
	tick(true)
	tick(true)
	if count != 1 {
		t.Fatalf("got %d, want one plug event", count)
	}
	m.Load(cfg)
	tick(true)
	if count != 1 {
		t.Fatal("reload repeated the plug event")
	}
	health(telemetry.StatusOffline)
	tick(false)
	health(telemetry.StatusOk)
	tick(true)
	if count != 1 {
		t.Fatal("recovery claimed a new plug-in")
	}
	tick(false)
	tick(true)
	if count != 2 {
		t.Fatal("next real plug-in was lost")
	}
}

func TestLostSessionProofRevokesLevelAndStartWithoutAPlugEvent(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	now := time.Unix(1700000000, 0)
	m.SetNowFn(func() time.Time { return now })
	b := events.NewBus()
	m.SetBus(b)
	count := 0
	b.Subscribe(events.KindChargingConnected, func(events.Event) { count++ })
	b.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{
		"charger": {Status: telemetry.StatusOk, LastSuccess: &now},
	}, Now: now})
	m.ObserveSession("garage", false, 0, 0, false, "easee:A", "")
	now = now.Add(3 * time.Second)
	m.ObserveSession("garage", true, 4300, 6000, true, "easee:A", "session-1")
	m.SetCurrentSoC("garage", .84)
	c := NewController(m, nil, nil, nil)
	c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
	if count != 1 {
		t.Fatalf("real plug-in events=%d, want 1", count)
	}
	// A terminal charger state withdraws its proof. The cable remains in,
	// and later waiting/paused samples cannot revive the previous Start.
	for i := 0; i < 3; i++ {
		now = now.Add(5 * time.Second)
		m.ObserveSession("garage", true, 0, 6000, false, "easee:A", "")
		c.restoreManualHoldForSession("garage")
		if s, _ := m.State("garage"); !s.PluggedIn || s.SoCSource != "assumed" || s.SoCRetention != "unavailable" || !s.ManualRestoreUnconfirmed {
			t.Fatalf("lost proof state=%+v", s)
		}
		if hold, ok := c.GetManualHold("garage", now); !ok || hold.PowerW != 0 {
			t.Fatalf("lost proof retained old Start: %+v %v", hold, ok)
		}
		if count != 1 {
			t.Fatalf("lost proof invented a plug-in: events=%d", count)
		}
	}
}
