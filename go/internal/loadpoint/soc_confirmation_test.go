package loadpoint

import "testing"

func TestSoCConfirmationDoesNotSurviveUnknownSession(t *testing.T) {
	cfg := []Config{{ID: "garage", DriverName: "evse", VehicleCapacityWh: 60000}}
	m := NewManager()
	m.Load(cfg)
	m.Observe("garage", true, 4300, 6000, true)
	assertSource := func(want string) {
		t.Helper()
		s, _ := m.State("garage")
		if s.SoCSource != want {
			t.Fatalf("source = %q, want %q", s.SoCSource, want)
		}
	}
	assertSource("assumed")
	if !m.SetCurrentSoC("garage", .84) {
		t.Fatal("input rejected")
	}
	assertSource("") // API supplies "inferred" for a confirmed anchor.
	m.Load(cfg)
	m.Observe("garage", true, 4300, 6600, true)
	assertSource("")
	s, _ := m.State("garage")
	if s.CurrentSoC < .849 || s.CurrentSoC > .851 {
		t.Fatalf("confirmed estimate lost on config reload: %v", s.CurrentSoC)
	}
	// Until the charger reports a stable session identity, a process restart
	// cannot prove the same car stayed connected. Never present the default as
	// a level inferred from the user's last input.
	m = NewManager()
	m.Load(cfg)
	m.Observe("garage", true, 4300, 6600, true)
	assertSource("assumed")
	m.AnchorVehicleSoC("garage", .85)
	assertSource("")
	m.Observe("garage", false, 0, 0, true)
	assertSource("")
	m.Observe("garage", true, 0, 0, true)
	assertSource("assumed")
}

func TestLowSoCCorrectionAfterEnergyWasDelivered(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000}})
	m.Observe("garage", true, 4300, 9000, true)
	m.SetCurrentSoC("garage", .05)
	s, _ := m.State("garage")
	if s.CurrentSoC < .049 || s.CurrentSoC > .051 {
		t.Fatalf("slider correction was ignored: %v", s.CurrentSoC)
	}
	m.Observe("garage", true, 4300, 9600, true)
	s, _ = m.State("garage")
	if s.CurrentSoC < .059 || s.CurrentSoC > .061 {
		t.Fatalf("energy must accrue from the corrected level: %v", s.CurrentSoC)
	}
}
