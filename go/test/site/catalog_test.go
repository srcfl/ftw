//go:build unix

package site

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// provenance says where each scenario comes from: the entries of FTW's
// field fault catalog it covers (reports/felkatalog/catalog.yaml in the FTW
// stack workspace) and its sources.
var provenance = map[string]struct {
	catalog []string
	sources []string
}{
	"s0-25":     {[]string{"home-070", "ocpp-011", "ocpp-013", "ui-052"}, []string{"https://github.com/srcfl/ftw/pull/1779", "https://github.com/srcfl/ftw/pull/1790"}},
	"s0b-25":    {[]string{"home-070", "ocpp-011", "ocpp-013", "ui-052"}, []string{"https://github.com/srcfl/ftw/pull/1790"}},
	"s1-16":     {nil, []string{"simulator test before beta 0.155.0, 21 Oct 2026"}},
	"s1-25":     {nil, []string{"simulator test before beta 0.155.0, 21 Oct 2026"}},
	"s2-16":     {[]string{"ev-078"}, []string{"https://github.com/srcfl/ftw/pull/1782"}},
	"s3-16":     {[]string{"ev-100"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s3-25":     {[]string{"ev-100"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s4-25":     {[]string{"ev-093"}, []string{"https://github.com/srcfl/ftw/pull/1774"}},
	"s4b-25":    {[]string{"ev-093"}, []string{"https://github.com/srcfl/ftw/pull/1774"}},
	"s5-25":     {[]string{"ev-089", "ev-110"}, []string{"https://github.com/srcfl/ftw/pull/1774"}},
	"s5s-25":    {[]string{"ev-089", "ev-102"}, []string{"https://github.com/srcfl/ftw/pull/1791"}},
	"s6-25":     {[]string{"home-061", "ocpp-010", "tel-035", "tel-037", "tel-038", "ui-052"}, []string{"https://github.com/srcfl/ftw/pull/1780"}},
	"s7-25":     {[]string{"dev-100", "ev-085", "ev-091", "home-061", "home-074", "hw-061"}, []string{"https://github.com/srcfl/ftw/pull/1778", "https://github.com/srcfl/ftw/pull/1787"}},
	"s7s-25":    {[]string{"ev-085", "ev-091"}, []string{"https://github.com/srcfl/ftw/pull/1778"}},
	"s8-60-25":  {[]string{"dev-076", "ev-099", "ev-107"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s8-120-25": {[]string{"dev-076", "ev-099", "ev-107"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s9-25":     {[]string{"ev-094"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s9r-25":    {nil, []string{"https://github.com/srcfl/ftw/pull/1800 (probe: a held 1p order raised to 3p beside L2 at 20 A)"}},
	"s10-25":    {[]string{"dev-076", "ev-105"}, []string{"https://github.com/srcfl/ftw/pull/1776"}},
	"s11-25":    {[]string{"ev-104"}, []string{"https://github.com/srcfl/ftw/pull/1763", "https://github.com/srcfl/ftw/pull/1793"}},
	"s5t-25":    {nil, []string{"https://github.com/srcfl/ftw/pull/1800 (probe: surplus-only with a goal)", "https://github.com/srcfl/ftw/pull/1799"}},
	"s12-20":    {[]string{"ev-097"}, []string{"https://github.com/srcfl/ftw/pull/1799", "field report, 22 Oct 2026, v0.155.0-beta.1"}},
	"s13-25":    {[]string{"ocpp-009", "dev-119"}, []string{"https://github.com/srcfl/ftw/issues/1788"}},
	"s14-25":    {nil, []string{"https://github.com/srcfl/ftw/pull/1800 (help1022 car1p, fix D)"}},
	"s15-25":    {nil, []string{"https://github.com/srcfl/ftw/pull/1804", "release blocker 22 Oct 2026: a one-phase charger gets 0 W under Charge now"}},
	"s15b-25":   {nil, []string{"https://github.com/srcfl/ftw/pull/1804", "release blocker 22 Oct 2026: a one-phase charger gets 0 W under Charge now (phase mode 1p)"}},
	"s15c-25":   {nil, []string{"https://github.com/srcfl/ftw/pull/1804", "release blocker 22 Oct 2026: a one-phase charger gets 0 W under Charge now (phase mode auto)"}},
	"s15bat-16": {nil, []string{"https://github.com/srcfl/ftw/pull/1805", "release blocker 22 Oct 2026: a one-phase charger never starts while Core raises its battery"}},
	"s15p-25":   {nil, []string{"https://github.com/srcfl/ftw/pull/1804", "release blocker 22 Oct 2026: a one-phase charger, planned charging"}},
}

func TestEveryScenarioSaysWhereItComesFrom(t *testing.T) {
	for _, sc := range scenarios {
		p, ok := provenance[sc.name]
		if !ok || len(p.catalog)+len(p.sources) == 0 {
			t.Errorf("%s has no catalog entry or source", sc.name)
		}
	}
	for name := range provenance {
		found := false
		for _, sc := range scenarios {
			found = found || sc.name == name
		}
		if !found {
			t.Errorf("provenance for %s, which is no scenario", name)
		}
	}
}

type catalogEntry struct {
	ID      string   `yaml:"id"`
	Kind    string   `yaml:"kind"`
	Area    string   `yaml:"area"`
	Title   string   `yaml:"title"`
	Status  string   `yaml:"status"`
	Sources []string `yaml:"sources"`
}

// TestCatalogCoverage reads the fault catalog at FTW_SITE_CATALOG and writes
// coverage.md: per area, which faults have a scenario here and which do not.
// It fails when a scenario names an entry the catalog does not have.
func TestCatalogCoverage(t *testing.T) {
	path := os.Getenv("FTW_SITE_CATALOG")
	if path == "" {
		t.Skip("set FTW_SITE_CATALOG to the fault catalog (catalog.yaml)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Entries []catalogEntry `yaml:"entries"`
	}
	if err := yaml.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalogEntry{}
	for _, e := range cat.Entries {
		byID[e.ID] = e
	}
	covered := map[string][]string{}
	for name, p := range provenance {
		for _, id := range p.catalog {
			if _, ok := byID[id]; !ok {
				t.Errorf("%s names catalog entry %s, which the catalog does not have", name, id)
			}
			covered[id] = append(covered[id], name)
		}
	}
	type tally struct{ faults, open, covered, openCovered int }
	areas := map[string]*tally{}
	var md strings.Builder
	md.WriteString("# Site-test coverage of the fault catalog\n\n")
	md.WriteString("| Entry | Status | Title | Scenarios |\n|---|---|---|---|\n")
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := byID[id]
		if e.Kind != "fault" {
			if len(covered[id]) > 0 {
				sort.Strings(covered[id])
				fmt.Fprintf(&md, "| %s | %s | %s | %s |\n", id, e.Kind, e.Title, strings.Join(covered[id], ", "))
			}
			continue
		}
		a := areas[e.Area]
		if a == nil {
			a = &tally{}
			areas[e.Area] = a
		}
		open := e.Status == "open" || e.Status == "partly_fixed" || e.Status == "fix_unmerged"
		a.faults++
		if open {
			a.open++
		}
		if len(covered[id]) > 0 {
			a.covered++
			if open {
				a.openCovered++
			}
			sort.Strings(covered[id])
			fmt.Fprintf(&md, "| %s | %s | %s | %s |\n", id, e.Status, e.Title, strings.Join(covered[id], ", "))
		}
	}
	md.WriteString("\n| Area | Faults | With a scenario | Open | Open with a scenario |\n|---|---:|---:|---:|---:|\n")
	names := make([]string, 0, len(areas))
	for a := range areas {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		x := areas[a]
		fmt.Fprintf(&md, "| %s | %d | %d | %d | %d |\n", a, x.faults, x.covered, x.open, x.openCovered)
	}
	out := os.Getenv("FTW_SITE_OUT")
	if out == "" {
		// No place to keep it: the log is the report.
		t.Logf("%d catalog entries, %d with a scenario\n\n%s", len(byID), len(covered), md.String())
		return
	}
	dst := filepath.Join(out, "coverage.md")
	if err := os.WriteFile(dst, []byte(md.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d catalog entries, %d with a scenario; wrote %s", len(byID), len(covered), dst)
}
