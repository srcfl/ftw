package drivers

import (
	"testing"
)

// Verification status is what lets the UI distinguish "production-ready"
// drivers from "ported but unproven" ones. This test parses the real
// drivers/ dir and asserts the expected status labels for each driver
// we've manually annotated. Every other driver in the tree is expected
// to parse as "experimental" (the normalized default for missing /
// unknown values).
func TestCatalogVerificationStatus(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	cases := []struct {
		id     string
		status string
	}{
		{"ferroamp", "production"},
		{"sungrow-shx", "production"},
		{"easee-cloud", "production"},
		{"ferroamp-modbus", "experimental"},
		{"sourceful-zap", "beta"},
		{"deye", "experimental"},
		{"solis", "experimental"},
		{"solis-string", "experimental"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			e, ok := byID[tc.id]
			if !ok {
				t.Fatalf("driver %q missing from catalog (got %d entries)", tc.id, len(entries))
			}
			if e.VerificationStatus != tc.status {
				t.Errorf("%s: VerificationStatus=%q, want %q", tc.id, e.VerificationStatus, tc.status)
			}
		})
	}
}

// Drivers at production status must also have a non-empty VerifiedBy
// list — otherwise the label is hearsay. Beta is fuzzier; experimental
// needs nothing. This check runs against the real catalog so
// adding a new "production" annotation without also filling in
// VerifiedBy fails loud at CI.
func TestCatalogProductionDriversHaveVerifier(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.VerificationStatus != "production" {
			continue
		}
		if len(e.VerifiedBy) == 0 {
			t.Errorf("%s (%s): marked production but no VerifiedBy entries — who tested it?",
				e.ID, e.Filename)
		}
		if e.VerifiedAt == "" {
			t.Errorf("%s (%s): marked production but no VerifiedAt date", e.ID, e.Filename)
		}
	}
}

// Unknown / garbage values in the Lua file must normalize to
// "experimental" rather than propagate an invalid label to the UI.
func TestNormalizeVerificationStatus(t *testing.T) {
	cases := map[string]string{
		"production":   "production",
		"PRODUCTION":   "production",
		"Beta":         "beta",
		"experimental": "experimental",
		"":             "experimental",
		"  ":           "experimental",
		"prod":         "experimental", // typo → safest default
		"alpha":        "experimental", // non-canonical → safest default
	}
	for in, want := range cases {
		if got := normalizeVerificationStatus(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCatalogConfigSecrets verifies the regex-based parser surfaces
// `config_secrets = { ... }` from a driver's DRIVER block end-to-end.
// Sonnen is the canonical user — its api_token has to land in the
// catalog so the Settings UI can render the password input.
func TestCatalogConfigSecrets(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	sonnen, ok := byID["sonnen"]
	if !ok {
		t.Fatalf("sonnen driver missing from catalog (got %d entries)", len(entries))
	}
	if got, want := sonnen.ConfigSecrets, []string{"api_token"}; len(got) != len(want) || got[0] != want[0] {
		t.Errorf("sonnen ConfigSecrets = %v, want %v", got, want)
	}

	// Drivers that don't declare config_secrets must come back with a
	// nil/empty slice — never a phantom entry from a regex over-match.
	if len(byID["pixii"].ConfigSecrets) != 0 {
		t.Errorf("pixii unexpectedly has ConfigSecrets=%v", byID["pixii"].ConfigSecrets)
	}
	if len(byID["ferroamp"].ConfigSecrets) != 0 {
		t.Errorf("ferroamp unexpectedly has ConfigSecrets=%v", byID["ferroamp"].ConfigSecrets)
	}
}
