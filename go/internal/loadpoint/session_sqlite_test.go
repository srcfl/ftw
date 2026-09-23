package loadpoint_test

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestConfirmedBatteryLevelSurvivesDatabaseCloseAndReopen(t *testing.T) {
	for _, capacityWh := range []float64{60000, 100000} {
		t.Run(fmt.Sprintf("capacity_%.0f", capacityWh), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg := []loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000}}
			m := loadpoint.NewManager()
			m.Load(cfg)
			m.SetSessionStore(store)
			m.ObserveSession("garage", true, 4300, 9000, true, "easee:TEST", "728:2026-01-01T08:00:00Z")
			if !m.SetCurrentSoC("garage", .84) {
				t.Fatal("level refused")
			}
			// The capacity endpoint applies its saved config through Manager.Load.
			// A correction must rewrite the durable anchor before the next restart.
			cfg[0].VehicleCapacityWh = capacityWh
			m.Load(cfg)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			m = loadpoint.NewManager()
			m.Load(cfg)
			m.SetSessionStore(store)
			m.ObserveSession("garage", true, 4300, 9600, true, "easee:TEST", "728:2026-01-01T08:00:00Z")
			s, _ := m.State("garage")
			if math.Abs(s.CurrentSoC-(.84+540/capacityWh)) > 1e-9 || s.SoCSource == "assumed" || s.SoCRetention != "session" {
				t.Fatalf("restart did not retain level: %+v", s)
			}
		})
	}
}

func TestDelayedCounterProgressSurvivesDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	newManager := func() *loadpoint.Manager {
		m := loadpoint.NewManager()
		m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 75000}})
		m.SetSessionStore(store)
		return m
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	m := newManager()
	m.SetNowFn(func() time.Time { return at })
	sample := loadpoint.EVSample{Connected: true, RequestActive: true, DeviceID: "easee:TEST", SessionID: "open-1", SessionWh: 1000, EnergyAt: at, PowerW: 6900, PowerAt: at}
	m.ObserveSample("garage", sample)
	m.SetCurrentSoC("garage", .76)
	for i := 0; i < 17; i++ {
		at = at.Add(5 * time.Second)
		sample.PowerAt = at
		m.ObserveSample("garage", sample)
	}
	// A stopped charge checkpoints immediately, including between periodic saves.
	sample.PowerW = 0
	m.ObserveSample("garage", sample)
	before, _ := m.State("garage")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m = newManager()
	m.SetNowFn(func() time.Time { return at })
	m.ObserveSample("garage", sample)
	after, _ := m.State("garage")
	if before.CurrentSoC != after.CurrentSoC || after.SoCRetention != "session" {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
}
