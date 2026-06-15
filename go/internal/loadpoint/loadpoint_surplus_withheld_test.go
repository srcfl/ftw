package loadpoint

import (
	"testing"
	"time"
)

// A surplus_only loadpoint paused below its 3-phase floor makes the charger
// report "not requesting current" (NCRQ). That stop is self-induced — we
// withheld power, the vehicle didn't decline — so it must NOT count toward
// session completion. Otherwise a cloudy spell below the floor would latch the
// session done and the planner would stop offering PV surplus for the rest of
// the day, never resuming when the sun returns.
func TestSelfWithheldNCRQDoesNotComplete(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "ctek",
		VehicleCapacityWh: 60000, PluginSoCPct: 20,
	}})
	m.SetTarget("garage", 80, time.Date(2026, 6, 8, 6, 0, 0, 0, time.UTC))

	clock := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return clock })

	// We are withholding power (surplus below the floor); charger reports NCRQ.
	m.SetSurplusWithheld("garage", true)
	m.Observe("garage", true, 0, 0, false)
	clock = clock.Add(5 * time.Minute) // well past the 90s completion timeout
	m.Observe("garage", true, 0, 0, false)

	if st, _ := m.State("garage"); st.SoCSource == "completed" {
		t.Errorf("self-withheld NCRQ must not latch session complete: %+v", st)
	}
}

// Once we stop withholding (surplus recovered, we offer power) and the vehicle
// STILL refuses for the full timeout, that is a genuine vehicle-side
// completion and the latch should fire — the self-withheld exemption must not
// leak past the pause.
func TestGenuineNCRQStillCompletesAfterWithheldClears(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "ctek",
		VehicleCapacityWh: 60000, PluginSoCPct: 20,
	}})
	m.SetTarget("garage", 80, time.Date(2026, 6, 8, 6, 0, 0, 0, time.UTC))

	clock := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return clock })

	// Withheld phase: NCRQ must not count.
	m.SetSurplusWithheld("garage", true)
	m.Observe("garage", true, 0, 0, false)
	clock = clock.Add(5 * time.Minute)
	m.Observe("garage", true, 0, 0, false)

	// Surplus recovered: we now offer power, but the vehicle keeps refusing.
	m.SetSurplusWithheld("garage", false)
	m.Observe("garage", true, 0, 0, false) // completion clock starts fresh here
	clock = clock.Add(2 * time.Minute)     // past 90s of genuine refusal
	m.Observe("garage", true, 0, 0, false)

	if st, _ := m.State("garage"); st.SoCSource != "completed" {
		t.Errorf("genuine NCRQ after withheld clears should complete: %+v", st)
	}
}
