package loadpoint

import "testing"

// TestResolvePhaseMode locks the phase_mode precedence — in particular the
// operator directive (2026-05-30) that an active charge schedule overrides the
// surplus 1Φ forecast lock so a deadline-driven charge can pull 3Φ grid power.
func TestResolvePhaseMode(t *testing.T) {
	cases := []struct {
		name           string
		operatorMode   string
		scheduleActive bool
		locked1P       bool
		surplusOn      bool
		dwell          string
		want           string
	}{
		// The fix: a schedule beats the 1Φ forecast lock.
		{"schedule overrides 1p lock (auto)", "auto", true, true, true, "1p", "auto"},
		{"schedule overrides 1p lock (unset)", "", true, true, true, "1p", "auto"},
		// An explicit operator pin is still honoured under a schedule.
		{"schedule honours explicit 3p", "3p", true, true, false, "", "3p"},
		{"schedule honours explicit 1p", "1p", true, true, false, "", "1p"},

		// No schedule: the surplus 1Φ lock applies as before.
		{"1p lock applies without schedule", "auto", false, true, true, "3p", "1p"},
		// A 3Φ-only charger must never be forced to 1Φ by the surplus
		// lock — it physically cannot trickle single-phase (e.g. CTEK).
		{"3p charger overrides 1p lock", "3p", false, true, true, "1p", "3p"},
		// No schedule, surplus-active, auto → dwell verdict.
		{"dwell verdict when surplus auto", "auto", false, false, true, "3p", "3p"},
		{"auto default when no dwell", "auto", false, false, true, "", "auto"},
		{"unset → auto under surplus", "", false, false, true, "", "auto"},
		// No schedule, no surplus → operator's mode verbatim.
		{"operator verbatim, no surplus", "auto", false, false, false, "", "auto"},
		{"operator explicit 1p verbatim", "1p", false, false, false, "", "1p"},
		{"operator unset verbatim", "", false, false, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePhaseMode(tc.operatorMode, tc.scheduleActive, tc.locked1P, tc.surplusOn, tc.dwell)
			if got != tc.want {
				t.Errorf("resolvePhaseMode(%q, sched=%v, locked1P=%v, surplus=%v, dwell=%q) = %q, want %q",
					tc.operatorMode, tc.scheduleActive, tc.locked1P, tc.surplusOn, tc.dwell, got, tc.want)
			}
		})
	}
}

// TestSurplusActive_ScheduleOverridesRuntimeClamp verifies that a committed
// charge schedule (with surplus_only OFF) disables the RUNTIME surplus clamp
// (the MPC grid-deferral guard), so the schedule's planned grid charge isn't
// throttled to live PV surplus. The explicit SurplusOnly config still wins.
// Operator directive 2026-05-30.
func TestSurplusActive_ScheduleOverridesRuntimeClamp(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := Config{ID: "lp1"} // SurplusOnly: false
	c.SetGridDeferred("lp1", true)

	if !c.surplusActive(cfg, Schedule{}) {
		t.Fatal("grid-deferred + no schedule should be surplus-active")
	}
	if c.surplusActive(cfg, Schedule{SoCPct: 50}) {
		t.Error("active schedule (surplus_only off) must disable the runtime surplus clamp")
	}
	cfg.SurplusOnly = true
	if !c.surplusActive(cfg, Schedule{SoCPct: 50}) {
		t.Error("explicit SurplusOnly config must stay surplus-active even with a schedule")
	}
}
