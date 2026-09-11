package loadpoint

import "testing"

// A capacity the car itself reported (OCPP 2.0.1 NotifyEVChargingNeeds) is
// session-scoped exactly like a vehicle profile: it steers SoC inference while
// the car is plugged in, survives a config hot-reload, and reverts on plug-out
// because the next car may be a different one.
func TestSetSessionCapacitySessionScoped(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	m.Observe("a", true, 7000, 0, true)

	if m.SetSessionCapacityWh("nope", 40000) {
		t.Fatal("unknown loadpoint should not apply")
	}
	if m.SetSessionCapacityWh("a", 0) {
		t.Fatal("a car that reported no capacity must not zero the loadpoint's own")
	}
	if !m.SetSessionCapacityWh("a", 40000) {
		t.Fatal("apply failed")
	}

	// 20 kWh delivered on a 40 kWh car = +50 points from the 30 % anchor.
	// With the configured 60 kWh it would only be +33.
	m.Observe("a", true, 7000, 20000, true)
	st, _ := m.State("a")
	if st.CurrentSoC < 0.79 || st.CurrentSoC > 0.81 {
		t.Fatalf("SoC should follow the reported 40 kWh capacity, got %v", st.CurrentSoC)
	}

	// Hot-reload mid-session keeps the car's figure, not the config's.
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	cfgs := m.Configs()
	if len(cfgs) != 1 || cfgs[0].VehicleCapacityWh != 40000 {
		t.Fatalf("reported capacity should survive config reload, got %+v", cfgs)
	}

	// Plug-out restores the loadpoint's own capacity.
	m.Observe("a", false, 0, 0, false)
	cfgs = m.Configs()
	if len(cfgs) != 1 || cfgs[0].VehicleCapacityWh != 60000 {
		t.Fatalf("capacity should restore to the configured 60000, got %+v", cfgs)
	}
}

// Measured outranks configured, whichever order the two arrive in: a vehicle
// profile is an operator's estimate of the car that usually parks here, the
// NotifyEVChargingNeeds figure is the car that is actually plugged in.
func TestReportedCapacityOutranksProfile(t *testing.T) {
	t.Run("profile first", func(t *testing.T) {
		m := NewManager()
		m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
		m.Observe("a", true, 7000, 0, true)

		m.ApplyVehicleProfile("a", "Leaf", 40000)
		m.SetSessionCapacityWh("a", 77000)

		if got := m.Configs()[0].VehicleCapacityWh; got != 77000 {
			t.Fatalf("the car's own capacity should win, got %v", got)
		}
		// Plug-out still restores the configured value, not the profile's.
		m.Observe("a", false, 0, 0, false)
		if got := m.Configs()[0].VehicleCapacityWh; got != 60000 {
			t.Fatalf("plug-out should restore the configured 60000, got %v", got)
		}
	})

	t.Run("car first", func(t *testing.T) {
		m := NewManager()
		m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
		m.Observe("a", true, 7000, 0, true)

		m.SetSessionCapacityWh("a", 77000)
		m.ApplyVehicleProfile("a", "Leaf", 40000)

		if got := m.Configs()[0].VehicleCapacityWh; got != 77000 {
			t.Fatalf("a later profile must not overwrite the car's own capacity, got %v", got)
		}
		// The profile still names the car for the UI.
		st, _ := m.State("a")
		if st.VehicleName != "Leaf" {
			t.Fatalf("vehicle name should still apply, got %+v", st)
		}
		m.Observe("a", false, 0, 0, false)
		if got := m.Configs()[0].VehicleCapacityWh; got != 60000 {
			t.Fatalf("plug-out should restore the configured 60000, got %v", got)
		}
	})
}
