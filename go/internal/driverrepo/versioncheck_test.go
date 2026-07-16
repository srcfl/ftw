package driverrepo

import (
	"strings"
	"testing"
)

func TestValidateDriverVersionChange(t *testing.T) {
	base := testDriver("1.0.0")
	changed := []byte(strings.Replace(string(base), "return 1000", "return 2000", 1))
	if err := ValidateDriverVersionChange("demo.lua", base, changed); err == nil {
		t.Fatal("runtime change without version bump accepted")
	}
	bumped := []byte(strings.Replace(string(changed), "version = \"1.0.0\"", "version = \"1.1.0\"", 1))
	if err := ValidateDriverVersionChange("demo.lua", base, bumped); err != nil {
		t.Fatal(err)
	}

	withoutHostAPI := []byte(strings.ReplaceAll(strings.ReplaceAll(string(base), "  host_api_min = 1,\n", ""), "  host_api_max = 1,\n", ""))
	if err := ValidateDriverVersionChange("demo.lua", withoutHostAPI, base); err != nil {
		t.Fatalf("host API metadata-only change requires bump: %v", err)
	}
}

func TestCompareSemverPrerelease(t *testing.T) {
	for _, tc := range []struct{ newer, older string }{
		{"1.0.0", "1.0.0-beta.2"},
		{"1.0.0-beta.11", "1.0.0-beta.2"},
		{"1.0.0-beta.2", "1.0.0-beta.1"},
		{"2.0.0", "1.99.99"},
	} {
		if compareSemver(tc.newer, tc.older) <= 0 {
			t.Fatalf("%s should be newer than %s", tc.newer, tc.older)
		}
	}
}
