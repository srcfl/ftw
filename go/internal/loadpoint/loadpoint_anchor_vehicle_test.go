package loadpoint

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestCachedVAGSoCReplacesManualPlanningAnchor(t *testing.T) {
	tel := telemetry.NewStore()
	soc := .78
	tel.Update("vag", telemetry.DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
	tel.EmitMetric("vag", "vehicle_soc_age_s", 34*60, "s", "", "")
	tel.DriverHealthMut("vag").RecordSuccess()
	_, receivedAt, _ := tel.LatestMetric("vag", "vehicle_soc_age_s")
	pick := telemetry.PickVehicleForAnchor(tel, false, receivedAt.Add(time.Second))
	if pick.Driver == "" {
		t.Fatal("timestamped VAG observation did not reach anchoring")
	}
	m := NewManager()
	m.Load([]Config{{ID: "easee", VehicleCapacityWh: 95000, PluginSoC: .27}})
	now := pick.UpdatedAt.Add(-time.Minute)
	m.SetNowFn(func() time.Time { return now })
	m.Observe("easee", true, 0, 0, true)
	now = pick.UpdatedAt
	m.Observe("easee", true, 0, 0, true)
	now = receivedAt
	m.Observe("easee", true, 0, 2000, true)
	m.SetCurrentSoC("easee", .27)
	for range 3 {
		if !m.AnchorVehicleSoCAt("easee", pick.SoC, pick.UpdatedAt) {
			t.Fatal("same-session VAG anchor refused")
		}
		st, _ := m.State("easee")
		want := .78 + 2000*DefaultChargeEfficiency/95000
		if math.Abs(st.CurrentSoC-want) > 1e-9 || st.SoCSource == "assumed" {
			t.Fatalf("planning still uses manual 27%% or double-counted energy: %+v, want %v", st, want)
		}
	}
}

// TestAnchorVehicleSoC — when a trusted vehicle BMS reading (e.g. Tesla
// via TeslaBLEProxy) is paired to a loadpoint, the control loop anchors
// the inferred SoC to it. After AnchorVehicleSoC the current_soc equals
// the BMS value, and any further delivered Wh advance from that anchor
// (so the estimate keeps tracking between BMS refreshes). This is the
// automatic counterpart to the operator's manual SetCurrentSoC.
func TestAnchorVehicleSoC(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.25}})
	// Plug in, deliver 9 kWh → naive estimate = 25 + 9000*0.9/60000*100 = 38.5 %.
	m.Observe("a", true, 7400, 9000, true)
	if st, _ := m.State("a"); math.Abs(st.CurrentSoC-0.385) > 1e-9 {
		t.Fatalf("pre-anchor SoC: got %.2f want 38.5%%", st.CurrentSoC)
	}
	// The bound vehicle's BMS reports the real SoC is 31 %.
	if !m.AnchorVehicleSoC("a", 0.31) {
		t.Fatal("AnchorVehicleSoC returned false on plugged-in loadpoint")
	}
	st, _ := m.State("a")
	if st.CurrentSoC < 0.305 || st.CurrentSoC > 0.315 {
		t.Errorf("post-anchor SoC: got %.2f want ~31", st.CurrentSoC)
	}
	// Deliver another 3 kWh → should be ~36 % (31 + 3000/60000*100).
	m.Observe("a", true, 7400, 12000, true)
	st, _ = m.State("a")
	if st.CurrentSoC < 0.35 || st.CurrentSoC > 0.37 {
		t.Errorf("after more delivery SoC: got %.2f want ~36", st.CurrentSoC)
	}
}

// TestAnchorVehicleSoCEveryTickStaysLocked — the control loop calls
// AnchorVehicleSoC every tick with the latest BMS reading. Even though
// Observe re-runs the inference each tick, the per-tick re-anchor keeps
// current_soc locked to the latest BMS value rather than drifting on the
// delivered-Wh estimate.
func TestAnchorVehicleSoCEveryTickStaysLocked(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.25}})
	m.Observe("a", true, 7400, 9000, true)
	// Tick 1: BMS says 31.
	m.AnchorVehicleSoC("a", 0.31)
	if st, _ := m.State("a"); st.CurrentSoC < 0.305 || st.CurrentSoC > 0.315 {
		t.Fatalf("tick1 SoC: got %.2f want ~31", st.CurrentSoC)
	}
	// Tick 2: more energy delivered AND BMS refreshes to 32. Observe runs
	// the inference first (as the controller does), then we re-anchor.
	m.Observe("a", true, 7400, 10000, true)
	m.AnchorVehicleSoC("a", 0.32)
	st, _ := m.State("a")
	if st.CurrentSoC < 0.315 || st.CurrentSoC > 0.325 {
		t.Errorf("tick2 SoC: got %.2f want ~32 (locked to latest BMS)", st.CurrentSoC)
	}
}

// TestAnchorVehicleSoCRejectsUnpluggedAndUnknown — a BMS reading is only
// meaningful during an active session, and an unknown id is a no-op.
func TestAnchorVehicleSoCRejectsUnpluggedAndUnknown(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	if m.AnchorVehicleSoC("a", 0.4) {
		t.Error("should reject AnchorVehicleSoC on never-plugged loadpoint")
	}
	if m.AnchorVehicleSoC("ghost", 0.4) {
		t.Error("should reject AnchorVehicleSoC on unknown id")
	}
	m.Observe("a", true, 0, 0, true)
	m.Observe("a", false, 0, 0, true)
	if m.AnchorVehicleSoC("a", 0.4) {
		t.Error("should reject AnchorVehicleSoC after unplug")
	}
}

// A cloud SoC arrives late. Anchoring it at its observation time keeps the
// energy delivered since then, instead of pinning the estimate to the old
// value.
func TestAnchorVehicleSoCAtAddsEnergySinceObservation(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.25}})
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	now := t0
	m.SetNowFn(func() time.Time { return now })
	for i, wh := range []float64{0, 1000, 2000, 3000, 4000, 5000, 6000, 7000} {
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		m.Observe("a", true, 11000, wh, true)
	}
	// The car read 50 % at t0+10 min, when 2000 Wh had been delivered.
	observed := t0.Add(10 * time.Minute)
	want := 0.50 + 5000*DefaultChargeEfficiency/60000
	for range 3 {
		if !m.AnchorVehicleSoCAt("a", 0.50, observed) {
			t.Fatal("anchor refused")
		}
		if st, _ := m.State("a"); math.Abs(st.CurrentSoC-want) > 1e-9 {
			t.Fatalf("SoC %.4f, want %.4f", st.CurrentSoC, want)
		}
	}
	// Between samples the energy is interpolated.
	if !m.AnchorVehicleSoCAt("a", 0.50, t0.Add(12*time.Minute+30*time.Second)) {
		t.Fatal("anchor refused")
	}
	if st, _ := m.State("a"); math.Abs(st.CurrentSoC-(0.50+4500*DefaultChargeEfficiency/60000)) > 1e-9 {
		t.Fatalf("interpolated SoC %.4f", st.CurrentSoC)
	}
	// Re-anchoring the same reading as energy grows moves the estimate on.
	now = now.Add(5 * time.Minute)
	m.Observe("a", true, 11000, 8000, true)
	m.AnchorVehicleSoCAt("a", 0.50, observed)
	if st, _ := m.State("a"); math.Abs(st.CurrentSoC-(want+1000*DefaultChargeEfficiency/60000)) > 1e-9 {
		t.Fatalf("estimate pinned to the reading: %.4f", st.CurrentSoC)
	}
}

func TestAnchorVehicleSoCAtBeforePlugIn(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.25}})
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	now := t0
	m.SetNowFn(func() time.Time { return now })
	m.Observe("a", true, 0, 0, true)
	now = t0.Add(10 * time.Minute)
	m.Observe("a", true, 11000, 1500, true)

	if m.AnchorVehicleSoCAt("a", 0.60, t0.Add(-anchorBeforePlugSlack-time.Minute)) {
		t.Fatal("a reading long before plug-in may predate a drive")
	}
	if !m.AnchorVehicleSoCAt("a", 0.60, t0.Add(-time.Minute)) {
		t.Fatal("a reading just before plug-in should anchor")
	}
	if st, _ := m.State("a"); math.Abs(st.CurrentSoC-(0.60+1500*DefaultChargeEfficiency/60000)) > 1e-9 {
		t.Fatalf("SoC %.4f", st.CurrentSoC)
	}
}

// After a restart the history starts mid-session. Energy at an earlier
// reading is unknown, so the reading must not anchor.
func TestAnchorVehicleSoCAtUnknownEnergyRefuses(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.25}})
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	now := t0
	m.SetNowFn(func() time.Time { return now })
	m.Observe("a", true, 11000, 4000, true)
	if m.AnchorVehicleSoCAt("a", 0.60, t0.Add(-2*time.Minute)) {
		t.Fatal("energy before the first sample is unknown")
	}
	if !m.AnchorVehicleSoCAt("a", 0.60, t0) {
		t.Fatal("a reading at the first sample should anchor")
	}
	// Samples older than the kept window are trimmed.
	for i := 1; i <= 30; i++ {
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		m.Observe("a", true, 11000, 4000+float64(i)*100, true)
	}
	if m.AnchorVehicleSoCAt("a", 0.60, t0) {
		t.Fatal("trimmed history must not anchor")
	}
}
