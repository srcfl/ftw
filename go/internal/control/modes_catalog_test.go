package control

import "testing"

// TestModeCatalogCoversAllModes is the UI-side counterpart to the HA
// discovery fix: the dashboard builds its mode buttons from ModeCatalog, so
// the catalog must describe exactly the canonical mode set — no missing mode
// (would vanish from the UI), no extra/typo key (would render a button that
// /api/mode rejects), no duplicate (would render twice).
func TestModeCatalogCoversAllModes(t *testing.T) {
	cat := ModeCatalog()
	if len(cat) != len(AllModes()) {
		t.Fatalf("ModeCatalog has %d entries, AllModes has %d", len(cat), len(AllModes()))
	}

	seen := map[Mode]int{}
	for _, info := range cat {
		seen[info.Key]++
		if !IsValidMode(info.Key) {
			t.Errorf("ModeCatalog entry %q is not a valid mode", info.Key)
		}
		if info.Label == "" {
			t.Errorf("ModeCatalog entry %q has an empty label", info.Key)
		}
		switch info.Tier {
		case TierPrimary, TierAdvanced, TierHidden:
		default:
			t.Errorf("ModeCatalog entry %q has unknown tier %q", info.Key, info.Tier)
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("ModeCatalog lists %q %d times, want exactly once", k, n)
		}
	}
	for _, m := range AllModes() {
		if seen[m] == 0 {
			t.Errorf("ModeCatalog is missing canonical mode %q", m)
		}
	}
}

// TestModeCatalogPrimaryIsArbitragePair locks the default-facing choices.
// These two planner modes are what a fresh install shows up front, and were
// the exact modes HA rejected before the discovery fix — keep them primary so
// the dashboard and the operator's mental model stay aligned.
func TestModeCatalogPrimaryIsArbitragePair(t *testing.T) {
	var primary []Mode
	for _, info := range ModeCatalog() {
		if info.Tier == TierPrimary {
			primary = append(primary, info.Key)
		}
	}
	want := []Mode{ModePlannerPassiveArbitrage, ModePlannerArbitrage}
	if len(primary) != len(want) {
		t.Fatalf("primary tier = %v, want %v", primary, want)
	}
	for i := range want {
		if primary[i] != want[i] {
			t.Errorf("primary[%d] = %q, want %q", i, primary[i], want[i])
		}
	}
}
