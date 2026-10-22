package loadpoint

import "testing"

// TestResolvePhaseMode locks the phase_mode precedence: an operator pin wins,
// unset means auto, a charger that cannot reach 6 A on three phases runs on
// one, planned charging runs on three (Fredrik, 22 Oct 2026: one phase never
// helps the fuse and caps the car near 3.7 kW), and only surplus charging
// without a schedule may use one phase. The 2026-05-30 directive stands: a
// schedule overrides the surplus 1Φ forecast lock.
func TestResolvePhaseMode(t *testing.T) {
	cases := []struct {
		name           string
		operatorMode   string
		oneOnly        bool // the charger cannot reach 6 A on three phases
		scheduleActive bool
		locked1P       bool
		surplusOn      bool
		dwell          string
		want           string
	}{
		// A schedule beats the 1Φ forecast lock: auto and unset are three
		// phases.
		{"schedule overrides 1p lock (auto)", "auto", false, true, true, true, "1p", "3p"},
		{"schedule overrides 1p lock (unset)", "", false, true, true, true, "1p", "3p"},
		// An explicit operator pin is still honoured under a schedule.
		{"schedule honours explicit 3p", "3p", false, true, true, false, "", "3p"},
		{"schedule honours explicit 1p", "1p", false, true, true, false, "", "1p"},

		// No schedule: the surplus 1Φ lock applies as before.
		{"1p lock applies without schedule", "auto", false, false, true, true, "3p", "1p"},
		// A 3Φ-only charger must never be forced to 1Φ by the surplus
		// lock — it physically cannot trickle single-phase (e.g. CTEK).
		{"3p charger overrides 1p lock", "3p", false, false, true, true, "1p", "3p"},
		// No schedule, surplus-active, auto → dwell verdict.
		{"dwell verdict when surplus auto", "auto", false, false, false, true, "3p", "3p"},
		{"auto default when no dwell", "auto", false, false, false, true, "", "auto"},
		{"unset → auto under surplus", "", false, false, false, true, "", "auto"},
		// No schedule, no surplus: auto and unset are three phases, a pin is
		// verbatim.
		{"auto is 3p without surplus", "auto", false, false, false, false, "", "3p"},
		{"unset is 3p without surplus", "", false, false, false, false, "", "3p"},
		{"operator explicit 1p verbatim", "1p", false, false, false, false, "", "1p"},
		// The day lock and the dwell belong to surplus charging alone.
		{"1p lock ignored without surplus", "auto", false, false, true, false, "1p", "3p"},
		// A one-phase-only charger runs on one phase, whatever drives it.
		{"one-phase charger, schedule", "auto", true, true, false, false, "", "1p"},
		{"one-phase charger, unset", "", true, false, false, false, "", "1p"},
		{"one-phase charger pinned 3p", "3p", true, false, false, false, "", "3p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePhaseMode(tc.operatorMode, !tc.oneOnly, tc.scheduleActive, tc.locked1P, tc.surplusOn, tc.dwell)
			if got != tc.want {
				t.Errorf("resolvePhaseMode(%q, one-phase=%v, sched=%v, locked1P=%v, surplus=%v, dwell=%q) = %q, want %q",
					tc.operatorMode, tc.oneOnly, tc.scheduleActive, tc.locked1P, tc.surplusOn, tc.dwell, got, tc.want)
			}
		})
	}
}

func TestSurplusActiveKeepsOnlyExplicitSolarPermission(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := Config{ID: "lp1"}
	if c.surplusActive(cfg, Schedule{}) || c.GridDeferred(cfg.ID) {
		t.Fatal("missing prices must not invent a solar-only choice")
	}
	if c.surplusActive(cfg, Schedule{SoC: .5}) {
		t.Fatal("saved goal must permit its grid budget")
	}
	cfg.SurplusOnly = true
	if !c.surplusActive(cfg, Schedule{SoC: .5}) {
		t.Fatal("explicit solar-only permission must remain")
	}
}
