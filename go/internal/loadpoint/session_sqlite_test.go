package loadpoint_test

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"

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
			if math.Abs(s.CurrentSoC-(.84+600/capacityWh)) > 1e-9 || s.SoCSource == "assumed" || s.SoCRetention != "session" {
				t.Fatalf("restart did not retain level: %+v", s)
			}
		})
	}
}
