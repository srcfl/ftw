package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestClampLoadWBounds(t *testing.T) {
	t.Parallel()
	if got := clampLoadW(-50, 0, 11000); got != 0 {
		t.Fatalf("negative load must floor at 0, got %v", got)
	}
	if got := clampLoadW(50000, 0, 11000); got != 11000 {
		t.Fatalf("50 kW must cut to fuse, got %v", got)
	}
	if got := clampLoadW(math.NaN(), 200, 11000); got != 200 {
		t.Fatalf("NaN must fall back to min, got %v", got)
	}
	if got := clampLoadW(700, 0, 11000); got != 700 {
		t.Fatalf("in-range load must stay, got %v", got)
	}
}

func TestCapSlotsLoadToFuse(t *testing.T) {
	t.Parallel()
	slots := capSlotsLoad([]Slot{
		{LoadW: 50000},
		{LoadW: 700},
		{LoadW: -10},
	}, 0, 11000)
	if slots[0].LoadW != 11000 {
		t.Fatalf("wild slot must cut to 11000 W, got %v", slots[0].LoadW)
	}
	if slots[1].LoadW != 700 {
		t.Fatalf("in-range slot must stay, got %v", slots[1].LoadW)
	}
	if slots[2].LoadW != 0 {
		t.Fatalf("negative slot must floor at 0, got %v", slots[2].LoadW)
	}
}

func TestRainCheckLoadFloorsHundredWattNight(t *testing.T) {
	t.Parallel()
	// 15 kWh recent day → mean 625 W → slot floor 125 W.
	// A full day with one 100 W night hour (the operator report) must
	// lift that hour without stretching the evening peak.
	slots := make([]Slot, 24)
	for i := range slots {
		slots[i] = Slot{LoadW: 600, LenMin: 60}
	}
	slots[3].LoadW = 100
	slots[19].LoadW = 2000
	slots = rainCheckLoadSlots(slots, 15000, 11000)
	if slots[3].LoadW < 120 {
		t.Fatalf("100 W night must lift toward recent mean, got %.0f W", slots[3].LoadW)
	}
	if slots[19].LoadW != 2000 {
		t.Fatalf("evening peak must stay, got %.0f W", slots[19].LoadW)
	}
}

func TestRainCheckLoadScalesCollapsedDay(t *testing.T) {
	t.Parallel()
	// 24 × 50 W = 1.2 kWh vs 15 kWh recent. After the 125 W floor
	// the day is 3 kWh, which is exactly 20% of 15 kWh — no extra scale.
	slots := make([]Slot, 24)
	for i := range slots {
		slots[i] = Slot{LoadW: 50, LenMin: 60}
	}
	slots = rainCheckLoadSlots(slots, 15000, 11000)
	got := forecastLoadWh(slots)
	if got < 2900 {
		t.Fatalf("collapsed day must lift to ~3 kWh, got %.0f Wh", got)
	}
	if got > 5000 {
		t.Fatalf("rain-check must not force-fit a full recent day, got %.0f Wh", got)
	}
}

func TestRainCheckLoadLeavesNormalDay(t *testing.T) {
	t.Parallel()
	slots := make([]Slot, 24)
	for i := range slots {
		slots[i] = Slot{LoadW: 600, LenMin: 60}
	}
	out := rainCheckLoadSlots(slots, 15000, 11000)
	if math.Abs(forecastLoadWh(out)-14400) > 1 {
		t.Fatalf("a 14.4 kWh forecast vs 15 kWh recent must stay, got %.0f Wh", forecastLoadWh(out))
	}
}

func TestRainCheckLoadNoHistoryNoOp(t *testing.T) {
	t.Parallel()
	slots := []Slot{{LoadW: 100, LenMin: 60}}
	out := rainCheckLoadSlots(slots, 0, 11000)
	if out[0].LoadW != 100 {
		t.Fatalf("no history must leave the slot alone, got %v", out[0].LoadW)
	}
}

func TestForecastLoadWhUsesRemainingHours(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
	slots := []Slot{{
		StartMs: start.UnixMilli(), LenMin: 15,
		ExecutionStartMs: start.Add(10 * time.Minute).UnixMilli(),
		LoadW:            1200,
	}}
	if got := forecastLoadWh(slots); math.Abs(got-100) > 1e-9 {
		t.Fatalf("rain-check energy=%g Wh, want 100 (5 min of 1200 W), not 300", got)
	}
	slots[0].ExecutionStartMs = 0
	if got := forecastLoadWh(slots); math.Abs(got-300) > 1e-9 {
		t.Fatalf("full slot energy=%g Wh, want 300", got)
	}
}

func TestRecentDailyLoadWhSkipsEmptyDays(t *testing.T) {
	st, err := state.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.Local)
	if got := recentDailyLoadWh(st, now, 3); got != 0 {
		t.Fatalf("empty store must return 0, got %v", got)
	}
}

func TestCapPlanLoadSetsNameplate(t *testing.T) {
	t.Parallel()
	plan := &Plan{Actions: []Action{{LoadW: 40000}, {LoadW: 800}}}
	capPlanLoad(plan, 0, 11000)
	if plan.LoadMaxW != 11000 {
		t.Fatalf("plan must carry load ceiling, got %v", plan.LoadMaxW)
	}
	if plan.Actions[0].LoadW != 11000 {
		t.Fatalf("published wild load must cut, got %v", plan.Actions[0].LoadW)
	}
	if plan.Actions[1].LoadW != 800 {
		t.Fatalf("in-range action must stay, got %v", plan.Actions[1].LoadW)
	}
}
