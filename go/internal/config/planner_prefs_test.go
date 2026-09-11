package config

import (
	"math"
	"testing"
)

func TestForecastTrustSafetyK(t *testing.T) {
	if got := ForecastTrustCautious.SafetyK(); got != 2 {
		t.Errorf("cautious k=%v, want 2", got)
	}
	if got := ForecastTrustBalanced.SafetyK(); got != 1 {
		t.Errorf("balanced k=%v, want 1", got)
	}
	if got := ForecastTrustBold.SafetyK(); got != 0 {
		t.Errorf("bold k=%v, want 0", got)
	}
	if got := ForecastTrust("").SafetyK(); got != 1 {
		t.Errorf("empty k=%v, want 1", got)
	}
}

func TestBatteryExportPlannerModeKey(t *testing.T) {
	if got := BatteryExportAllowed.PlannerModeKey(); got != "planner_arbitrage" {
		t.Errorf("allowed → %s, want planner_arbitrage", got)
	}
	for _, e := range []BatteryExport{BatteryExportUnknown, BatteryExportNotAllowed, ""} {
		if got := e.PlannerModeKey(); got != "planner_passive_arbitrage" {
			t.Errorf("%q → %s, want planner_passive_arbitrage", e, got)
		}
	}
}

func TestDeriveBatteryExport(t *testing.T) {
	cases := []struct {
		mode string
		want BatteryExport
	}{
		{"planner_arbitrage", BatteryExportUnknown},
		{"planner_passive_arbitrage", BatteryExportNotAllowed},
		{"planner_self", BatteryExportNotAllowed},
		{"planner_cheap", BatteryExportNotAllowed},
		{"idle", BatteryExportUnknown},
		{"", BatteryExportUnknown},
	}
	for _, tc := range cases {
		if got := DeriveBatteryExport(tc.mode); got != tc.want {
			t.Errorf("mode %q → %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestResolvePlannerPrefsStoredWins(t *testing.T) {
	trust, export, k, missing := ResolvePlannerPrefs("bold", "allowed", "0.85", "planner_passive_arbitrage", "cautious", "not_allowed", nil)
	if export != BatteryExportAllowed || missing {
		t.Fatalf("got export=%s missing=%v", export, missing)
	}
	// The float is the primitive: it wins over the stored enum outright and
	// the enum answer is re-derived from it.
	if k != 0.85 {
		t.Errorf("k=%v, want 0.85", k)
	}
	if trust != ForecastTrustBalanced {
		t.Errorf("trust=%s, want balanced (derived from k=0.85)", trust)
	}
}

func TestResolvePlannerPrefsEnumOnlySiteKeepsItsK(t *testing.T) {
	// A site upgraded from the three-step slider has no planner_safety_k
	// row. Its plan must not move: k resolves to the step's own value and
	// missingStored asks the caller to write the float.
	for _, tc := range []struct {
		trust string
		wantK float64
	}{{"cautious", 2}, {"balanced", 1}, {"bold", 0}} {
		trust, _, k, missing := ResolvePlannerPrefs(tc.trust, "allowed", "", "planner_passive_arbitrage", "", "", nil)
		if k != tc.wantK {
			t.Errorf("%s → k=%v, want %v", tc.trust, k, tc.wantK)
		}
		if string(trust) != tc.trust {
			t.Errorf("%s → trust=%s", tc.trust, trust)
		}
		if !missing {
			t.Errorf("%s: missing k row must be persisted", tc.trust)
		}
	}
}

func TestResolvePlannerPrefsRejectsJunkK(t *testing.T) {
	// A corrupt row falls back to the enum rather than poisoning the DP.
	_, _, k, missing := ResolvePlannerPrefs("cautious", "allowed", "banana", "planner_passive_arbitrage", "", "", nil)
	if k != 2 || !missing {
		t.Fatalf("junk k: got k=%v missing=%v, want 2/true", k, missing)
	}
	// Out-of-range rows clamp instead of failing.
	_, _, k, _ = ResolvePlannerPrefs("", "allowed", "9", "planner_passive_arbitrage", "", "", nil)
	if k != 2 {
		t.Errorf("k=9 → %v, want clamp to 2", k)
	}
	_, _, k, _ = ResolvePlannerPrefs("", "allowed", "-3", "planner_passive_arbitrage", "", "", nil)
	if k != 0 {
		t.Errorf("k=-3 → %v, want clamp to 0", k)
	}
}

func TestClampSafetyK(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0, 0}, {0.85, 0.85}, {2, 2}, {-0.1, 0}, {2.5, 2},
	}
	for _, tc := range cases {
		if got := ClampSafetyK(tc.in); got != tc.want {
			t.Errorf("ClampSafetyK(%v)=%v, want %v", tc.in, got, tc.want)
		}
	}
	if got := ClampSafetyK(math.NaN()); got != SafetyKDefault {
		t.Errorf("NaN → %v, want %v", got, SafetyKDefault)
	}
	if got := FormatSafetyK(0.85); got != "0.85" {
		t.Errorf("FormatSafetyK(0.85)=%q", got)
	}
	if _, ok := ParseSafetyK(""); ok {
		t.Error("empty row must not parse")
	}
}

func TestResolvePlannerPrefsActiveUpgradeAsks(t *testing.T) {
	trust, export, k, missing := ResolvePlannerPrefs("", "", "", "planner_arbitrage", "", "", nil)
	if trust != ForecastTrustBalanced {
		t.Errorf("trust=%s, want balanced", trust)
	}
	if k != 1 {
		t.Errorf("k=%v, want 1", k)
	}
	if export != BatteryExportUnknown {
		t.Errorf("export=%s, want unknown (must confirm)", export)
	}
	if !missing {
		t.Error("empty sqlite should be missingStored")
	}
}

func TestResolvePlannerPrefsPassiveStaysOff(t *testing.T) {
	_, export, _, _ := ResolvePlannerPrefs("", "", "", "planner_passive_arbitrage", "", "", nil)
	if export != BatteryExportNotAllowed {
		t.Errorf("export=%s, want not_allowed", export)
	}
}

func TestPlannerEffectiveSafetyKIsTheStoredK(t *testing.T) {
	// The slider owns the live haircut: a YAML k no longer wins (it only
	// seeds the first boot, see TestResolvePlannerPrefsSeedsFromYAMLK).
	yaml := 0.5
	p := &Planner{PVForecastSafetyK: &yaml}
	if got := p.EffectiveSafetyK(2); got != 2 {
		t.Errorf("stored 2 → 2 despite YAML k, got %v", got)
	}
	if got := p.EffectiveSafetyK(0.85); got != 0.85 {
		t.Errorf("fractional k must survive, got %v", got)
	}
	empty := &Planner{}
	if got := empty.EffectiveSafetyK(3); got != 2 {
		t.Errorf("out-of-range k=%v, want clamp to 2", got)
	}
}

func TestParseForecastTrustRejectsJunk(t *testing.T) {
	if _, ok := ParseForecastTrust("spicy"); ok {
		t.Fatal("spicy must not parse")
	}
	if _, ok := ParseBatteryExport("maybe"); ok {
		t.Fatal("maybe must not parse")
	}
}

func TestResolvePlannerPrefsSeedsFromYAMLK(t *testing.T) {
	// No stored pref, no yaml forecast_trust, but a legacy
	// pv_forecast_safety_k: the k seeds the float exactly and the nearest
	// trust step alongside it, once — the slider owns it from then on.
	two := 2.0
	trust, _, k, missing := ResolvePlannerPrefs("", "", "", "planner_passive_arbitrage", "", "", &two)
	if trust != ForecastTrustCautious || k != 2 || !missing {
		t.Fatalf("k=2 seed: got trust=%s k=%v missing=%v", trust, k, missing)
	}
	// A YAML k off the three steps is not snapped to one.
	seven := 0.7
	trust, _, k, _ = ResolvePlannerPrefs("", "", "", "planner_passive_arbitrage", "", "", &seven)
	if k != 0.7 {
		t.Fatalf("k=0.7 seed must stay 0.7, got %v", k)
	}
	if trust != ForecastTrustBalanced {
		t.Fatalf("k=0.7 → trust=%s, want balanced", trust)
	}
	// A stored pref always beats the yaml k.
	trust, _, k, _ = ResolvePlannerPrefs("bold", "allowed", "", "planner_passive_arbitrage", "", "", &two)
	if trust != ForecastTrustBold || k != 0 {
		t.Fatalf("stored beats yaml k: got trust=%s k=%v", trust, k)
	}
	// A stored float beats both.
	_, _, k, _ = ResolvePlannerPrefs("bold", "allowed", "1.75", "planner_passive_arbitrage", "", "", &two)
	if k != 1.75 {
		t.Fatalf("stored float must win, got %v", k)
	}
}
