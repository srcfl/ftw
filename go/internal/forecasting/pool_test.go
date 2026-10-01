package forecasting

import (
	"fmt"
	"testing"
	"time"
)

// scoredAt scores one series' forecast against 1,000 W PV and 1,000 W load.
func scoredAt(series, issue string, start, origin int64, pvW, loadW float64) ErrorSample {
	e := testError(series, "cfg", issue, float64(start), float64(origin), 1000-pvW, 1000-loadW)
	e.Prediction.PVW, e.Prediction.LoadW = pvW, loadW
	e.Daylight = true
	return e
}

func TestPoolFrozenSeriesCountsEachTargetHourOnce(t *testing.T) {
	start := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC).UnixMilli()
	var samples []ErrorSample
	for h := int64(0); h < 60; h++ {
		at := start + h*hourMS
		// Alternate lead buckets, so each bucket alone holds only 30 hours.
		ahead := 2 * hourMS
		if h%2 == 1 {
			ahead = 4 * hourMS
		}
		issue := fmt.Sprintf("issue-%d", h)
		samples = append(samples, scoredAt("energyplan", issue, at, at-ahead, 900, 1100),
			scoredAt("legacy_shadow", issue, at, at-ahead, 700, 1600))
	}
	// The first target again, from an earlier issue in a third bucket.
	samples = append(samples, scoredAt("energyplan", "early", start, start-8*hourMS, 900, 1100),
		scoredAt("legacy_shadow", "early", start, start-8*hourMS, 700, 1600))
	for _, m := range CompareFrozenSeries(samples, "energyplan", "legacy_shadow") {
		if m.Signal == "load" && m.Samples > 30 {
			t.Fatalf("fixture does not split the hours across buckets: %+v", m)
		}
	}
	m := PoolFrozenSeries(samples, "energyplan", "legacy_shadow", "load")
	if m.Samples != 61 || m.Hours != 60 || m.Days != 3 || m.ChampionMAEW != 100 || m.CandidateMAEW != 600 {
		t.Fatalf("pooled load comparison = %+v", m)
	}
	if pv := PoolFrozenSeries(samples, "energyplan", "legacy_shadow", "pv_daylight"); pv.ChampionMAEW != 100 || pv.CandidateMAEW != 300 {
		t.Fatalf("pooled PV comparison = %+v", pv)
	}
}

func TestComposeFrozenSeriesTakesEachSignalFromItsSeries(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	pv := scoredAt("energyplan", "issue", start, start-2*hourMS, 900, 1100)
	pv.Prediction.PVSource, pv.Prediction.LoadSource = "energyplan", "energyplan"
	load := scoredAt("legacy_shadow", "issue", start, start-2*hourMS, 700, 1600)
	load.Prediction.PVSource, load.Prediction.LoadSource = "legacy", "legacy"
	alone := scoredAt("legacy_shadow", "other", start+hourMS, start-hourMS, 700, 1600)
	got := ComposeFrozenSeries([]ErrorSample{pv, load, alone}, "energyplan", "legacy_shadow", "mix")
	if len(got) != 1 {
		t.Fatalf("composed %d samples, want only the target both series scored", len(got))
	}
	e := got[0]
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.Series != "mix" || e.PVErrorW != 100 || e.LoadErrorW != -600 || e.Prediction.PVW != 900 || e.Prediction.LoadW != 1600 ||
		e.Prediction.PVSource != "energyplan" || e.Prediction.LoadSource != "legacy" {
		t.Fatalf("mix did not take PV and load from their series: %+v", e)
	}
	if net, known, _, _ := signalError(e, "net"); !known || net != -700 {
		t.Fatalf("mixed net error = %v (known %v), want -700", net, known)
	}
	load.IssueID = "newer"
	if got := ComposeFrozenSeries([]ErrorSample{pv, load}, "energyplan", "legacy_shadow", "mix"); len(got) != 0 {
		t.Fatalf("errors from different issues were mixed: %+v", got)
	}
}
