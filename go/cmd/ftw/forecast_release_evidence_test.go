package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

const releaseFerroamp = "c04d137d595ba50b8c6178c82d917b115dbe9a7cbd2cf671ef2660e871f96de3"
const releaseSungrow = "a11ddc8de433c527a3d63cd014d243b09f3393ee3afa9b8e1594d4de8de37562"
const releaseSolarEdge = "94b5f18fb42c1413540d7321bca47ed755c8a0a9b069f8d51dbac4384910c496"

func TestForecastReleaseRequiresReviewedCodeAndPhysicalReleaseConfig(t *testing.T) {
	for _, tc := range []struct {
		name, digest string
		settings     map[string]any
		want         bool
	}{
		{"Ferroamp missing release", releaseFerroamp, nil, false},
		{"Ferroamp zero release", releaseFerroamp, map[string]any{"pplim_release_w": 0}, false},
		{"Ferroamp subwatt becomes zero", releaseFerroamp, map[string]any{"pplim_release_w": 0.5}, false},
		{"Ferroamp declared ceiling", releaseFerroamp, map[string]any{"pplim_release_w": 15000}, true},
		{"Ferroamp Lua numeric string", releaseFerroamp, map[string]any{"pplim_release_w": " 15000 "}, true},
		{"Ferroamp invalid number", releaseFerroamp, map[string]any{"pplim_release_w": math.Inf(1)}, false},
		{"Ferroamp NaN", releaseFerroamp, map[string]any{"pplim_release_w": math.NaN()}, false},
		{"Sungrow explicit disable", releaseSungrow, nil, true},
		{"Sungrow feedin missing ceiling", releaseSungrow, map[string]any{"pv_curtail_method": "feed_in"}, false},
		{"Sungrow feedin ceiling", releaseSungrow, map[string]any{"pv_curtail_method": "feed_in", "feed_in_release_w": 11000}, true},
		{"Sungrow register overflow", releaseSungrow, map[string]any{"pv_curtail_method": "feed_in", "feed_in_release_w": 100000}, false},
		{"SolarEdge explicit enablebit release", releaseSolarEdge, nil, true},
		{"unknown revision", "unknown", map[string]any{"pplim_release_w": 15000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := forecastReviewedRelease(tc.digest, tc.settings); got != tc.want {
				t.Fatalf("release evidence=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestForecastReleaseEvidenceFreezesConfigurationAndRejectsCustomReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ferroamp.lua")
	cfg := &config.Config{Drivers: []config.Driver{{Name: "pv", Lua: path, SupportsPVCurtail: true, Config: map[string]any{"pplim_release_w": 15000}}}}
	catalog := []drivers.CatalogEntry{{Path: "drivers/ferroamp.lua", Filename: "ferroamp.lua", Capabilities: []string{"pv"}, ArtifactSHA256: releaseFerroamp}}
	reads := 0
	digest := func(got string) (string, error) {
		reads++
		if got != path {
			t.Fatalf("hashed %q instead of resolved host path", got)
		}
		return releaseFerroamp, nil
	}
	evidence := forecastReleaseEvidenceWithDigest(cfg, catalog, digest)
	cfg.Drivers[0].Config["pplim_release_w"] = 0
	if !evidence("pv") || evidence("other") || reads != 1 {
		t.Fatal("lookup changed or performed extra source reads")
	}
	if forecastReleaseEvidenceWithDigest(cfg, catalog, digest)("pv") {
		t.Fatal("new config retained obsolete release ceiling")
	}
	cfg.Drivers[0].Config["pplim_release_w"] = 15000
	if err := os.WriteFile(path, []byte("-- custom driver with the same name\nfunction driver_command() return true end"), 0600); err != nil {
		t.Fatal(err)
	}
	if forecastReleaseEvidence(cfg, catalog)("pv") {
		t.Fatal("catalog artifact hash or name vouched for changed Lua bytes")
	}
	if forecastReleaseEvidenceWithDigest(cfg, nil, digest)("pv") {
		t.Fatal("unknown catalog qualified")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*config.Driver)
	}{
		{"disabled", func(d *config.Driver) { d.Disabled = true }},
		{"observe only", func(d *config.Driver) { d.ObserveOnly = true }},
		{"telemetry only", func(d *config.Driver) { d.BatteryTelemetryOnly = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *cfg
			copy.Drivers = append([]config.Driver(nil), cfg.Drivers...)
			tc.mutate(&copy.Drivers[0])
			if forecastReleaseEvidenceWithDigest(&copy, catalog, digest)("pv") {
				t.Fatal("inactive controller qualified")
			}
		})
	}
	if forecastReleaseEvidenceWithDigest(cfg, catalog, func(string) (string, error) { return "", fmt.Errorf("missing") })("pv") {
		t.Fatal("unreadable source qualified")
	}
}

func TestForecastReleaseScriptDigestUsesExactResolvedBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "driver.lua")
	data := []byte("return true\n")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := forecastReleaseScriptDigest(p)
	want := sha256.Sum256(data)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest=%s err=%v", got, err)
	}
	for _, invalid := range []string{"driver.lua", filepath.Dir(p), p + ".missing"} {
		if _, err := forecastReleaseScriptDigest(invalid); err == nil {
			t.Fatalf("invalid path accepted: %s", invalid)
		}
	}
	if err := os.WriteFile(p, make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := forecastReleaseScriptDigest(p); err == nil {
		t.Fatal("unbounded driver source accepted")
	}
}
