package config

import "testing"

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
	trust, export, missing := ResolvePlannerPrefs("bold", "allowed", "planner_passive_arbitrage", "cautious", "not_allowed")
	if trust != ForecastTrustBold || export != BatteryExportAllowed || missing {
		t.Fatalf("got trust=%s export=%s missing=%v", trust, export, missing)
	}
}

func TestResolvePlannerPrefsActiveUpgradeAsks(t *testing.T) {
	trust, export, missing := ResolvePlannerPrefs("", "", "planner_arbitrage", "", "")
	if trust != ForecastTrustBalanced {
		t.Errorf("trust=%s, want balanced", trust)
	}
	if export != BatteryExportUnknown {
		t.Errorf("export=%s, want unknown (must confirm)", export)
	}
	if !missing {
		t.Error("empty sqlite should be missingStored")
	}
}

func TestResolvePlannerPrefsPassiveStaysOff(t *testing.T) {
	_, export, _ := ResolvePlannerPrefs("", "", "planner_passive_arbitrage", "", "")
	if export != BatteryExportNotAllowed {
		t.Errorf("export=%s, want not_allowed", export)
	}
}

func TestPlannerEffectiveSafetyKYAMLWins(t *testing.T) {
	k := 0.5
	p := &Planner{PVForecastSafetyK: &k}
	if got := p.EffectiveSafetyK(ForecastTrustCautious); got != 0.5 {
		t.Errorf("yaml k=%v, want 0.5", got)
	}
	if !p.YAMLCustomK() {
		t.Error("YAMLCustomK should be true")
	}
	empty := &Planner{}
	if got := empty.EffectiveSafetyK(ForecastTrustCautious); got != 2 {
		t.Errorf("mapped k=%v, want 2", got)
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
