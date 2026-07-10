package loadpoint

import "testing"

// TestAnchorVehicleSoC — when a trusted vehicle BMS reading (e.g. Tesla
// via TeslaBLEProxy) is paired to a loadpoint, the control loop anchors
// the inferred SoC to it. After AnchorVehicleSoC the current_soc equals
// the BMS value, and any further delivered Wh advance from that anchor
// (so the estimate keeps tracking between BMS refreshes). This is the
// automatic counterpart to the operator's manual SetCurrentSoC.
func TestAnchorVehicleSoC(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 25}})
	// Plug in, deliver 9 kWh → naive estimate = 25 + 9000/60000*100 = 40 %.
	m.Observe("a", true, 7400, 9000, true)
	if st, _ := m.State("a"); st.CurrentSoCPct < 39 || st.CurrentSoCPct > 41 {
		t.Fatalf("pre-anchor SoC: got %.2f want ~40", st.CurrentSoCPct)
	}
	// The bound vehicle's BMS reports the real SoC is 31 %.
	if !m.AnchorVehicleSoC("a", 31) {
		t.Fatal("AnchorVehicleSoC returned false on plugged-in loadpoint")
	}
	st, _ := m.State("a")
	if st.CurrentSoCPct < 30.5 || st.CurrentSoCPct > 31.5 {
		t.Errorf("post-anchor SoC: got %.2f want ~31", st.CurrentSoCPct)
	}
	// Deliver another 3 kWh → should be ~36 % (31 + 3000/60000*100).
	m.Observe("a", true, 7400, 12000, true)
	st, _ = m.State("a")
	if st.CurrentSoCPct < 35 || st.CurrentSoCPct > 37 {
		t.Errorf("after more delivery SoC: got %.2f want ~36", st.CurrentSoCPct)
	}
}

// TestAnchorVehicleSoCEveryTickStaysLocked — the control loop calls
// AnchorVehicleSoC every tick with the latest BMS reading. Even though
// Observe re-runs the inference each tick, the per-tick re-anchor keeps
// current_soc locked to the latest BMS value rather than drifting on the
// delivered-Wh estimate.
func TestAnchorVehicleSoCEveryTickStaysLocked(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 25}})
	m.Observe("a", true, 7400, 9000, true)
	// Tick 1: BMS says 31.
	m.AnchorVehicleSoC("a", 31)
	if st, _ := m.State("a"); st.CurrentSoCPct < 30.5 || st.CurrentSoCPct > 31.5 {
		t.Fatalf("tick1 SoC: got %.2f want ~31", st.CurrentSoCPct)
	}
	// Tick 2: more energy delivered AND BMS refreshes to 32. Observe runs
	// the inference first (as the controller does), then we re-anchor.
	m.Observe("a", true, 7400, 10000, true)
	m.AnchorVehicleSoC("a", 32)
	st, _ := m.State("a")
	if st.CurrentSoCPct < 31.5 || st.CurrentSoCPct > 32.5 {
		t.Errorf("tick2 SoC: got %.2f want ~32 (locked to latest BMS)", st.CurrentSoCPct)
	}
}

// TestAnchorVehicleSoCRejectsUnpluggedAndUnknown — a BMS reading is only
// meaningful during an active session, and an unknown id is a no-op.
func TestAnchorVehicleSoCRejectsUnpluggedAndUnknown(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	if m.AnchorVehicleSoC("a", 40) {
		t.Error("should reject AnchorVehicleSoC on never-plugged loadpoint")
	}
	if m.AnchorVehicleSoC("ghost", 40) {
		t.Error("should reject AnchorVehicleSoC on unknown id")
	}
	m.Observe("a", true, 0, 0, true)
	m.Observe("a", false, 0, 0, true)
	if m.AnchorVehicleSoC("a", 40) {
		t.Error("should reject AnchorVehicleSoC after unplug")
	}
}
