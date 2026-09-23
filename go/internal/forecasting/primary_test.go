package forecasting

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPrimarySourcesRoundTripWithoutChangingKnownOrQuality(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	p := testPoint(start, start+hourMS, 0, 600)
	p.PVSource, p.LoadSource = "energyplan", "legacy"
	p.PVQuality, p.LoadQuality = "cold_start", "cold_start"
	issue := testIssue("mixed", "cfg", "champion", start-hourMS, start-hourMS, []Point{p})
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	var restored Issue
	if err := json.Unmarshal(raw, &restored); err != nil || restored.Validate() != nil {
		t.Fatalf("mixed primary did not survive archive: %v, %+v", err, restored)
	}
	if restored.Series[0].Points[0] != p {
		t.Fatalf("source or cold-start quality changed: %+v", restored.Series[0].Points[0])
	}
	for _, field := range []string{"pv", "load"} {
		bad := p
		if field == "pv" {
			bad.PVSource = strings.Repeat("x", 81)
		} else {
			bad.LoadSource = strings.Repeat("x", 81)
		}
		if validPoint(bad) {
			t.Fatalf("unbounded %s source accepted", field)
		}
	}
	p.PVSource, p.LoadSource = "", ""
	if !validPoint(p) {
		t.Fatal("historical record without source rejected")
	}
}

func TestFrozenComparisonScoresMixedPrimaryAndKnownColdZero(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	primary := testPoint(start, start+hourMS, 0, 600)
	primary.PVSource, primary.LoadSource = "energyplan", "legacy"
	primary.PVQuality, primary.LoadQuality = "cold_start", "cold_start"
	shadow := testPoint(start, start+hourMS, 500, 600)
	shadow.PVSource, shadow.LoadSource = "legacy", "legacy"
	issue := testIssue("mixed", "cfg", "champion", start-hourMS, start-hourMS, []Point{primary})
	issue.Series = append(issue.Series, Series{Name: "legacy_shadow", ModelVersion: "v1", Points: []Point{shadow}})
	truth := testObservation("cfg", start, start+hourMS, 0, 800)
	samples := Errors([]Issue{issue}, []Observation{truth}, start+hourMS)
	metrics := CompareFrozenSeries(samples, "champion", "legacy_shadow")
	if len(metrics) != 3 {
		t.Fatalf("known cold-start zero or default load lost: %+v", metrics)
	}
	for _, m := range metrics {
		if m.Signal == "pv" && (m.ChampionMAEW != 0 || m.CandidateMAEW != 500 || m.DeltaMAEW != 500) {
			t.Fatalf("primary/shadow roles reversed: %+v", m)
		}
		if m.Signal == "load" && (m.ChampionMAEW != 200 || m.CandidateMAEW != 200 || m.CandidateWinRate != .5) {
			t.Fatalf("shared legacy load fallback must tie: %+v", m)
		}
	}
	issue.Series[0].Points[0].PVKnown = false
	issue.Series[0].Points[0].PVQuality = "unknown"
	metrics = CompareFrozenSeries(Errors([]Issue{issue}, []Observation{truth}, start+hourMS), "champion", "legacy_shadow")
	if len(metrics) != 1 || metrics[0].Signal != "load" {
		t.Fatalf("unknown numeric zero was scored as available PV: %+v", metrics)
	}
}

func TestFrozenComparisonRejectsUnmatchedCapture(t *testing.T) {
	start := float64(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli())
	primary := testError("champion", "cfg", "capture", start, start-2*float64(hourMS), 100, 100)
	shadow := primary
	shadow.Series = "legacy_shadow"
	for name, change := range map[string]func(*ErrorSample){
		"different issue":       func(e *ErrorSample) { e.IssueID = "other" },
		"different origin":      func(e *ErrorSample) { e.OriginMS-- },
		"different receipt":     func(e *ErrorSample) { e.IssuedAtMS++ },
		"different site config": func(e *ErrorSample) { e.ConfigVersion = "new" },
	} {
		t.Run(name, func(t *testing.T) {
			other := shadow
			change(&other)
			if got := CompareFrozenSeries([]ErrorSample{primary, other}, "champion", "legacy_shadow"); len(got) != 0 {
				t.Fatalf("unmatched captures produced comparison: %+v", got)
			}
		})
	}
	newer := primary
	newer.OriginMS, newer.IssuedAtMS = primary.OriginMS+1, primary.IssuedAtMS+1
	newer.IssueID = "newer"
	if got := CompareFrozenSeries([]ErrorSample{primary, shadow, newer}, "champion", "legacy_shadow"); len(got) != 0 {
		t.Fatalf("new primary paired with stale shadow: %+v", got)
	}
}
