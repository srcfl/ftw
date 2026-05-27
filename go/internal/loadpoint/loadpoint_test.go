package loadpoint

import (
	"testing"
	"time"
)

func TestLoadPopulatesAndPreservesOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", DriverName: "easee-cloud", MaxChargeW: 11000},
		{ID: "street", DriverName: "zap", MaxChargeW: 7400},
	})
	if ids := m.IDs(); len(ids) != 2 || ids[0] != "garage" || ids[1] != "street" {
		t.Errorf("IDs not insertion-ordered: %v", ids)
	}
}

func TestLoadSkipsBlankID(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "", DriverName: "ghost"}, {ID: "real"}})
	if len(m.IDs()) != 1 || m.IDs()[0] != "real" {
		t.Errorf("blank ID should be skipped; got %v", m.IDs())
	}
}

func TestReloadPreservesObservedState(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "easee-cloud",
		VehicleCapacityWh: 60000, PluginSoCPct: 40,
	}})
	m.Observe("garage", true, 7400, 1200, true) // 1.2 kWh into session
	target := time.Date(2026, 4, 18, 6, 0, 0, 0, time.UTC)
	m.SetTarget("garage", 80, target)

	// Reload with same ID — state should persist.
	m.Load([]Config{{
		ID: "garage", DriverName: "easee-cloud", MaxChargeW: 11000,
		VehicleCapacityWh: 60000, PluginSoCPct: 40,
	}})
	st, ok := m.State("garage")
	if !ok {
		t.Fatal("state missing after reload")
	}
	// SoC = 40 + 1200/60000*100 = 42
	if !st.PluggedIn || st.CurrentPowerW != 7400 {
		t.Errorf("observed state lost: %+v", st)
	}
	if got := st.CurrentSoCPct; got < 41.5 || got > 42.5 {
		t.Errorf("SoC estimate: got %.2f, want ~42", got)
	}
	if st.TargetSoCPct != 80 || !st.TargetTime.Equal(target) {
		t.Errorf("target lost: %+v", st)
	}
	// But config should update.
	if st.MaxChargeW != 11000 {
		t.Errorf("config not updated: MaxChargeW=%f", st.MaxChargeW)
	}
}

func TestReloadDropsRemovedIDs(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a"}, {ID: "b"}})
	m.Load([]Config{{ID: "b"}})
	if ids := m.IDs(); len(ids) != 1 || ids[0] != "b" {
		t.Errorf("removed ID should be dropped; got %v", ids)
	}
}

func TestObserveOnUnknownIsNoop(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "real"}})
	m.Observe("ghost", true, 7400, 0, true) // must not panic
	if _, ok := m.State("ghost"); ok {
		t.Error("ghost state should not exist")
	}
}

func TestSetTargetClamp(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a"}})
	m.SetTarget("a", 250, time.Time{})
	st, _ := m.State("a")
	if st.TargetSoCPct != 100 {
		t.Errorf("should clamp to 100; got %f", st.TargetSoCPct)
	}
	m.SetTarget("a", -10, time.Time{})
	st, _ = m.State("a")
	if st.TargetSoCPct != 0 {
		t.Errorf("should clamp to 0; got %f", st.TargetSoCPct)
	}
}

// TestObserveUnpluggedClearsSoCEstimate — when the car is disconnected
// we can't meaningfully estimate its SoC, so the manager clears it.
// Otherwise a stale 42% would hang on the display after the car drove
// away.
func TestObserveUnpluggedClearsSoCEstimate(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 30}})
	m.Observe("a", true, 7400, 1800, true) // charging → SoC = 30 + 3 = 33
	if st, _ := m.State("a"); st.CurrentSoCPct < 32.5 || st.CurrentSoCPct > 33.5 {
		t.Fatalf("expected ~33 %% while plugged in, got %.2f", st.CurrentSoCPct)
	}
	m.Observe("a", false, 0, 0, true)
	if st, _ := m.State("a"); st.CurrentSoCPct != 0 || st.PluggedIn {
		t.Errorf("expected cleared state when unplugged, got %+v", st)
	}
}

// TestObserveNewSessionAnchor — on plug-in the anchor resets to
// Config.PluginSoCPct. This prevents residual session_wh from a
// previous session leaking into the new one.
func TestObserveNewSessionAnchor(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 50}})
	m.Observe("a", true, 0, 0, true)
	if st, _ := m.State("a"); st.CurrentSoCPct < 49 || st.CurrentSoCPct > 51 {
		t.Errorf("fresh plug-in should show 50 %%, got %.2f", st.CurrentSoCPct)
	}
	// Disconnect.
	m.Observe("a", false, 0, 0, true)
	// Re-plug — session delivered counter starts fresh.
	m.Observe("a", true, 0, 0, true)
	if st, _ := m.State("a"); st.CurrentSoCPct < 49 || st.CurrentSoCPct > 51 {
		t.Errorf("re-plug should re-anchor at 50 %%, got %.2f", st.CurrentSoCPct)
	}
}

// TestSetCurrentSoCReAnchors — operator corrects the inferred SoC
// mid-session. After SetCurrentSoC, current_soc equals the provided
// value, and any further delivered Wh advance from that anchor.
// Chargers can't read vehicle BMS, so this is the only way the
// operator can correct drift.
func TestSetCurrentSoCReAnchors(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 25}})
	// Plug in, deliver 9 kWh → naive estimate = 25 + 9000/60000*100 = 40 %.
	m.Observe("a", true, 7400, 9000, true)
	if st, _ := m.State("a"); st.CurrentSoCPct < 39 || st.CurrentSoCPct > 41 {
		t.Fatalf("pre-correction SoC: got %.2f want ~40", st.CurrentSoCPct)
	}
	// Operator looks at their dashboard: car is actually 60 %.
	if !m.SetCurrentSoC("a", 60) {
		t.Fatal("SetCurrentSoC returned false on plugged-in loadpoint")
	}
	st, _ := m.State("a")
	if st.CurrentSoCPct < 59 || st.CurrentSoCPct > 61 {
		t.Errorf("post-correction SoC: got %.2f want ~60", st.CurrentSoCPct)
	}
	// Deliver another 3 kWh → should be ~65 % (60 + 3000/60000*100).
	m.Observe("a", true, 7400, 12000, true)
	st, _ = m.State("a")
	if st.CurrentSoCPct < 64 || st.CurrentSoCPct > 66 {
		t.Errorf("after more delivery SoC: got %.2f want ~65", st.CurrentSoCPct)
	}
}

// TestSetCurrentSoCRejectsUnplugged — SoC is meaningless without an
// active session. Chargers may cling to the last known vehicle state
// briefly after unplug; blocking this avoids anchoring against noise.
func TestSetCurrentSoCRejectsUnplugged(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	if m.SetCurrentSoC("a", 55) {
		t.Error("should reject SetCurrentSoC on never-plugged loadpoint")
	}
	m.Observe("a", true, 0, 0, true)
	m.Observe("a", false, 0, 0, true)
	if m.SetCurrentSoC("a", 55) {
		t.Error("should reject SetCurrentSoC after unplug")
	}
}

func TestSetCurrentSoCClampsRange(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	m.Observe("a", true, 0, 0, true)
	m.SetCurrentSoC("a", 150)
	if st, _ := m.State("a"); st.CurrentSoCPct != 100 {
		t.Errorf("clamp high: got %.2f", st.CurrentSoCPct)
	}
	m.SetCurrentSoC("a", -10)
	if st, _ := m.State("a"); st.CurrentSoCPct != 0 {
		t.Errorf("clamp low: got %.2f", st.CurrentSoCPct)
	}
}

func TestStatesReturnsAllInOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", MaxChargeW: 11000, VehicleCapacityWh: 60000},
		{ID: "street", MaxChargeW: 7400, VehicleCapacityWh: 60000},
	})
	m.Observe("garage", true, 11000, 500, true)
	states := m.States()
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
	if states[0].ID != "garage" || states[1].ID != "street" {
		t.Errorf("wrong ordering: %v, %v", states[0].ID, states[1].ID)
	}
	if !states[0].PluggedIn {
		t.Error("garage should be plugged in")
	}
	if states[1].PluggedIn {
		t.Error("street should not be plugged in")
	}
}

// TestSessionCompletionSnapsToTarget walks the scenario where the
// vehicle charges normally, then explicitly stops requesting current
// for a sustained window (typically because it hit its own onboard
// SoC target or its onboard schedule ended). Without the completion
// latch the loadpoint would keep reporting a stale inferred SoC and
// the MPC would keep allocating PV surplus to a phantom sink, spilling
// it to the grid. With the latch the inferred SoC snaps to the target
// and the planner sees the EV as done.
func TestSessionCompletionSnapsToTarget(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "evse-test",
		VehicleCapacityWh: 60000, PluginSoCPct: 20, MaxChargeW: 11000,
	}})
	m.SetTarget("garage", 60, time.Date(2026, 5, 27, 15, 0, 0, 0, time.UTC))

	clock := time.Date(2026, 5, 27, 10, 19, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return clock })

	// Tick 1: connected and charging — request_active = true.
	m.Observe("garage", true, 7400, 0, true)
	if st, _ := m.State("garage"); st.SoCSource != "" {
		t.Errorf("session start should not be marked completed: %+v", st)
	}

	// Tick 2 (T+1m of charging): some energy delivered, inferred SoC rises.
	clock = clock.Add(60 * time.Second)
	m.Observe("garage", true, 7400, 1000, true) // SoC = 20 + 1000/60000*100 ≈ 21.67
	if st, _ := m.State("garage"); st.CurrentSoCPct < 21 || st.CurrentSoCPct > 22.5 {
		t.Errorf("expected inferred SoC ~21.67, got %.2f", st.CurrentSoCPct)
	}

	// Tick 3 (T+1m6s): vehicle stops requesting current.
	// delivered_wh frozen, power drops to 0.
	clock = clock.Add(6 * time.Second)
	m.Observe("garage", true, 0, 1000, false)
	if st, _ := m.State("garage"); st.SoCSource == "completed" {
		t.Errorf("first not-requesting tick should NOT yet complete (under threshold): %+v", st)
	}

	// Tick 4 (T+30s of not-requesting): below threshold — still not completed.
	clock = clock.Add(30 * time.Second)
	m.Observe("garage", true, 0, 1000, false)
	if st, _ := m.State("garage"); st.SoCSource == "completed" {
		t.Errorf("30s not-requesting should NOT yet complete (under 90s threshold): %+v", st)
	}

	// Tick 5 (T+90s of not-requesting): threshold reached — snap SoC to target.
	clock = clock.Add(60 * time.Second)
	m.Observe("garage", true, 0, 1000, false)
	st, _ := m.State("garage")
	if st.SoCSource != "completed" {
		t.Errorf("expected SoCSource='completed' after threshold, got %q (state=%+v)", st.SoCSource, st)
	}
	if st.CurrentSoCPct != 60 {
		t.Errorf("expected inferred SoC pinned to target 60, got %.2f", st.CurrentSoCPct)
	}

	// Tick 6: a transient request_active=true blip (EVSE retried and
	// briefly succeeded, or some other flicker). The latch must NOT
	// release — once the vehicle has declined this session, a brief
	// EVSE retry shouldn't reopen the allocation. Only a plug-out
	// clears it.
	clock = clock.Add(15 * time.Second)
	m.Observe("garage", true, 0, 1000, true)
	if st, _ := m.State("garage"); st.SoCSource != "completed" || st.CurrentSoCPct != 60 {
		t.Errorf("brief request_active flicker should not clear latch: %+v", st)
	}

	// Plug-out clears the latch fully.
	clock = clock.Add(10 * time.Second)
	m.Observe("garage", false, 0, 0, false)
	if st, _ := m.State("garage"); st.SoCSource != "" || st.PluggedIn {
		t.Errorf("plug-out should clear completion latch and SoCSource: %+v", st)
	}
}

// TestSessionCompletionRequiresTarget asserts the latch is gated on a
// configured target — an opportunistic loadpoint with no schedule
// should not snap to 0 (which would surprise an operator who left a
// car plugged in expecting overnight slow-charge from cheap grid).
func TestSessionCompletionRequiresTarget(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "evse-test",
		VehicleCapacityWh: 60000, PluginSoCPct: 20,
	}})
	// No SetTarget call — targetSoCPct stays 0.

	clock := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return clock })

	m.Observe("garage", true, 0, 0, false) // start with not-requesting
	clock = clock.Add(2 * time.Minute)     // well past 90s threshold
	m.Observe("garage", true, 0, 0, false)

	st, _ := m.State("garage")
	if st.SoCSource == "completed" {
		t.Errorf("completion should not trigger without a target: %+v", st)
	}
}

// TestRequestActiveDefaultPreservesInference asserts that drivers
// which always pass request_active=true (i.e. drivers without the
// vehicle-side-refusal signal) keep their pre-fix behaviour: SoC
// inference runs normally and nothing snaps to target.
func TestRequestActiveDefaultPreservesInference(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "evse-test",
		VehicleCapacityWh: 60000, PluginSoCPct: 30,
	}})
	m.SetTarget("garage", 80, time.Date(2026, 4, 18, 6, 0, 0, 0, time.UTC))

	clock := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return clock })

	// Plug in + charge — request_active=true the entire time (driver
	// has no completion signal to surface).
	m.Observe("garage", true, 0, 0, true)
	clock = clock.Add(5 * time.Minute)
	m.Observe("garage", true, 7400, 600, true) // 600 Wh in → SoC = 30 + 1
	st, _ := m.State("garage")
	if st.SoCSource == "completed" {
		t.Errorf("request_active=true must not trigger completion: %+v", st)
	}
	if st.CurrentSoCPct < 30.5 || st.CurrentSoCPct > 31.5 {
		t.Errorf("inferred SoC drifted: %.2f", st.CurrentSoCPct)
	}
}
