package mpc

import (
	"testing"
	"time"
)

// planWithActions builds a minimal fresh Plan whose Actions start at
// `start` in 15-minute slots with the given per-slot loadpoint watts
// under id "garage" (multi-LP map shape).
func planWithActions(start time.Time, lpW []float64) *Plan {
	actions := make([]Action, len(lpW))
	for i, w := range lpW {
		actions[i] = Action{
			SlotStartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin:  15,
		}
		if w > 0 {
			actions[i].LoadpointPowerW = map[string]float64{"garage": w}
		}
	}
	return &Plan{GeneratedAtMs: time.Now().UnixMilli(), Actions: actions}
}

// TestLoadpointPlanWindowsMergesContiguousSlots asserts that adjacent
// allocated slots come back as one window, past slots are dropped, a
// window cap keeps the Wh total intact, and the current slot is
// included with its full bounds.
func TestLoadpointPlanWindowsMergesContiguousSlots(t *testing.T) {
	now := time.Now().UTC().Truncate(15 * time.Minute)
	start := now.Add(-30 * time.Minute)
	// Slots: [past 11 kW] [past 0] [current 4 kW] [4 kW] [0] [11 kW]
	svc := &Service{}
	svc.InstallPlan(*planWithActions(start, []float64{11000, 0, 4000, 4000, 0, 11000}), Params{}, "garage")

	windows, totalWh := svc.LoadpointPlanWindows("garage", now.Add(1*time.Minute), 1)
	if len(windows) != 1 {
		t.Fatalf("want 1 window (max=1), got %d: %+v", len(windows), windows)
	}
	w := windows[0]
	if !w.Start.Equal(now) {
		t.Errorf("window start: want %v (current slot start), got %v", now, w.Start)
	}
	if !w.End.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("window end: want %v, got %v", now.Add(30*time.Minute), w.End)
	}
	if w.EnergyWh != 2000 {
		t.Errorf("window Wh: want 2000 (2×4000 W×0.25 h), got %v", w.EnergyWh)
	}
	// Total covers the capped-away 11 kW slot too: 2000 + 2750.
	if totalWh != 4750 {
		t.Errorf("total Wh: want 4750, got %v", totalWh)
	}

	// Uncapped: the far 11 kW slot becomes its own second window.
	windows, _ = svc.LoadpointPlanWindows("garage", now.Add(1*time.Minute), 0)
	if len(windows) != 2 {
		t.Fatalf("want 2 windows uncapped, got %d: %+v", len(windows), windows)
	}
	if windows[1].EnergyWh != 2750 {
		t.Errorf("second window Wh: want 2750, got %v", windows[1].EnergyWh)
	}
}

// TestLoadpointPlanWindowsLegacySingleLP asserts the legacy plan shape
// (Action.LoadpointW + Service.lastLoadpointID) is honoured, and that
// an unknown id sees nothing.
func TestLoadpointPlanWindowsLegacySingleLP(t *testing.T) {
	now := time.Now().UTC().Truncate(15 * time.Minute)
	p := &Plan{GeneratedAtMs: time.Now().UnixMilli(), Actions: []Action{
		{SlotStartMs: now.UnixMilli(), SlotLenMin: 15, LoadpointW: 6000},
	}}
	svc := &Service{}
	svc.InstallPlan(*p, Params{}, "carport")

	windows, totalWh := svc.LoadpointPlanWindows("carport", now, 0)
	if len(windows) != 1 || totalWh != 1500 {
		t.Fatalf("legacy shape: want 1 window / 1500 Wh, got %+v / %v", windows, totalWh)
	}
	if windows, totalWh = svc.LoadpointPlanWindows("other", now, 0); len(windows) != 0 || totalWh != 0 {
		t.Fatalf("unknown id: want nothing, got %+v / %v", windows, totalWh)
	}
}

// TestLoadpointPlanWindowsStalePlan asserts a plan older than
// MaxPlanAge promises no start times — same cutoff SlotDirectiveAt
// applies before the control loop falls back.
func TestLoadpointPlanWindowsStalePlan(t *testing.T) {
	now := time.Now().UTC().Truncate(15 * time.Minute)
	p := planWithActions(now, []float64{4000})
	p.GeneratedAtMs = time.Now().Add(-MaxPlanAge - time.Minute).UnixMilli()
	svc := &Service{}
	svc.InstallPlan(*p, Params{}, "garage")
	if windows, totalWh := svc.LoadpointPlanWindows("garage", now, 0); len(windows) != 0 || totalWh != 0 {
		t.Fatalf("stale plan: want nothing, got %+v / %v", windows, totalWh)
	}
}
