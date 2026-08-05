package loadpoint

import "testing"

// A vehicle profile is session-scoped: applied when the charging transaction
// identifies the car, steering SoC inference with that car's capacity, and
// reverted on plug-out because the next car may be a different one.
func TestApplyVehicleProfileSessionScoped(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 30}})
	m.Observe("a", true, 7000, 0, true)

	if m.ApplyVehicleProfile("nope", "Leaf", 40000) {
		t.Fatal("unknown loadpoint should not apply")
	}
	if !m.ApplyVehicleProfile("a", "Leaf", 40000) {
		t.Fatal("apply failed")
	}
	st, _ := m.State("a")
	if st.VehicleName != "Leaf" {
		t.Fatalf("vehicle name missing from state: %+v", st)
	}

	// 20 kWh delivered on a 40 kWh car = +50 points from the 30 % anchor.
	// With the configured 60 kWh it would only be +33 — the applied
	// capacity must win.
	m.Observe("a", true, 7000, 20000, true)
	st, _ = m.State("a")
	if st.CurrentSoCPct < 79 || st.CurrentSoCPct > 81 {
		t.Fatalf("SoC should follow the applied 40 kWh capacity, got %v", st.CurrentSoCPct)
	}

	// Hot-reload mid-session: the identified car survives.
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 30}})
	st, _ = m.State("a")
	if st.VehicleName != "Leaf" {
		t.Fatalf("vehicle should survive config reload, got %+v", st)
	}

	// Plug-out reverts to the loadpoint's own capacity.
	m.Observe("a", false, 0, 0, false)
	st, _ = m.State("a")
	if st.VehicleName != "" {
		t.Fatalf("vehicle should clear on plug-out: %+v", st)
	}
	cfgs := m.Configs()
	if len(cfgs) != 1 || cfgs[0].VehicleCapacityWh != 60000 {
		t.Fatalf("capacity should restore to the configured 60000, got %+v", cfgs)
	}
}
