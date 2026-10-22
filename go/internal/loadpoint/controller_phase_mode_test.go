package loadpoint

import "testing"

// TestResolvePhaseMode locks the phase_mode precedence: an operator pin wins,
// planned charging runs on three phases (Fredrik, 22 Oct 2026: one phase
// never helps the fuse and caps the car near 3.7 kW), and only surplus
// charging without a schedule may use one phase. The 2026-05-30 directive
// stands: a schedule overrides the surplus 1Φ forecast lock.
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
		// A schedule beats the 1Φ forecast lock: auto is three phases, unset
		// is the driver's own default.
		{"schedule overrides 1p lock (auto)", "auto", true, true, true, "1p", "3p"},
		{"schedule overrides 1p lock (unset)", "", true, true, true, "1p", ""},
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
		// No schedule, no surplus: auto is three phases, a pin is verbatim.
		{"auto is 3p without surplus", "auto", false, false, false, "", "3p"},
		{"operator explicit 1p verbatim", "1p", false, false, false, "", "1p"},
		{"operator unset verbatim", "", false, false, false, "", ""},
		// The day lock and the dwell belong to surplus charging alone.
		{"1p lock ignored without surplus", "auto", false, true, false, "1p", "3p"},
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
