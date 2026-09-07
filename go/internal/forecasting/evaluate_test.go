package forecasting

import (
	"math"
	"sort"
	"testing"
	"time"
)

func testObservation(config string, start, end int64, pv, load float64) Observation {
	return Observation{
		StartMS: start, EndMS: end, AvailableAtMS: end, PVW: pv, LoadW: load,
		PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: config,
	}
}

func testError(series, config, issueID string, start, origin, pvError, loadError float64) ErrorSample {
	startMS := int64(start)
	originMS := int64(origin)
	prediction := testPoint(startMS, startMS+hourMS, 100, 1000)
	return ErrorSample{
		Series: series, ConfigVersion: config, IssueID: issueID, OriginMS: originMS, IssuedAtMS: originMS,
		StartMS: startMS, EndMS: startMS + hourMS, AvailableAtMS: startMS + hourMS,
		Lead: LeadBucket(originMS, startMS), PVErrorW: pvError, LoadErrorW: loadError,
		PVKnown: true, LoadKnown: true, Prediction: prediction,
	}
}

func TestErrorsFindsMatchingObservationAmongConfigVersions(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	issue := testIssue("issue", "wanted", "champion", start-2*hourMS, start-hourMS, []Point{testPoint(start, start+hourMS, 100, 900)})
	wrong := testObservation("other", start, start+hourMS, 900, 9000)
	wanted := testObservation("wanted", start, start+hourMS, 200, 1000)
	for name, observations := range map[string][]Observation{"wrong first": {wrong, wanted}, "wanted first": {wanted, wrong}} {
		t.Run(name, func(t *testing.T) {
			errors := Errors([]Issue{issue}, observations, start+hourMS)
			if len(errors) != 1 || errors[0].PVErrorW != 100 || errors[0].LoadErrorW != 100 {
				t.Fatalf("matching truth was not scored: %+v", errors)
			}
		})
	}
}

func TestErrorsUsesDeterministicTieBreakForSameOrigin(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	origin := start - 2*hourMS
	a := testIssue("a", "cfg", "champion", origin, origin, []Point{testPoint(start, start+hourMS, 100, 900)})
	z := testIssue("z", "cfg", "champion", origin, origin, []Point{testPoint(start, start+hourMS, 300, 1100)})
	observation := testObservation("cfg", start, start+hourMS, 200, 1000)
	for _, issues := range [][]Issue{{a, z}, {z, a}} {
		errors := Errors(issues, []Observation{observation}, start+hourMS)
		if len(errors) != 1 || errors[0].IssueID != "z" || errors[0].PVErrorW != -100 {
			t.Fatalf("same-origin tie depends on input order: %+v", errors)
		}
	}
}

func TestErrorSampleValidateRejectsCalibrationPoison(t *testing.T) {
	start := float64(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli())
	errorSample := testError("champion", "cfg", "i", start, start-2*float64(hourMS), 10, 20)
	if err := errorSample.Validate(); err != nil {
		t.Fatalf("valid sample rejected: %v", err)
	}
	errorSample.PVErrorW = math.NaN()
	if err := errorSample.Validate(); err == nil {
		t.Fatal("nonfinite error must be rejected")
	}
	errorSample.PVErrorW = 10
	errorSample.Lead++
	if err := errorSample.Validate(); err == nil {
		t.Fatal("wrong lead bucket must be rejected")
	}
}

func TestCalibratorDeduplicatesAndRequiresSevenDays(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	history := make([]ErrorSample, 0, 120)
	sixDays := make([]ErrorSample, 0, 96)
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base + int64(day*24+hour)*hourMS
			latest := testError("champion", "cfg", "latest", float64(start), float64(start-2*hourMS), float64(day*10+hour), 0)
			older := latest
			older.IssueID = "older"
			older.OriginMS -= hourMS / 2
			older.IssuedAtMS = older.OriginMS
			older.Lead = LeadBucket(older.OriginMS, older.StartMS)
			history = append(history, latest, older)
			if day < 6 {
				sixDays = append(sixDays, latest, older)
			}
		}
	}
	sort.Slice(history, func(i, j int) bool { return history[i].IssueID > history[j].IssueID })
	origin := base + 8*24*hourMS
	calibrator := NewCalibrator(history, "cfg", origin)
	band := calibrator.Band("champion", "pv", origin+2*hourMS, 1000)
	if band.Method != BandMethodEmpirical || band.Samples != 56 || band.Days != 7 {
		t.Fatalf("deduplicated seven-day calibration = %+v", band)
	}
	before := band
	history[0].PVErrorW = 1e9
	if after := calibrator.Band("champion", "pv", origin+2*hourMS, 1000); after != before {
		t.Fatalf("calibrator changed after input mutation: before=%+v after=%+v", before, after)
	}
	cold := NewCalibrator(sixDays, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 1000)
	if cold.Method != BandMethodColdStart || cold.Days >= 7 {
		t.Fatalf("six dates claimed calibration: %+v", cold)
	}
}

func TestCalibratorKeepsOnlyLatest256HourlyTargets(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	history := make([]ErrorSample, 0, 400)
	for i := 0; i < 400; i++ {
		start := base + int64(i)*hourMS
		history = append(history, testError("champion", "cfg", "i", float64(start), float64(start-2*hourMS), float64(i), 0))
	}
	origin := base + 500*hourMS
	band := NewCalibrator(history, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 1000)
	if band.Samples != 256 || band.Days < 7 {
		t.Fatalf("bounded calibration cell = %+v", band)
	}
}

func TestCalibratorCalibratesQuarterHoursWithoutMixingDurations(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	quarterMS := int64(15 * time.Minute / time.Millisecond)
	history := make([]ErrorSample, 0, 120)
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base + int64(day*24+hour)*hourMS
			quarter := testError("champion", "cfg", "quarter-latest", float64(start), float64(start-2*hourMS), 40, 20)
			quarter.EndMS = start + quarterMS
			quarter.AvailableAtMS = quarter.EndMS
			quarter.Prediction.EndMS = quarter.EndMS
			older := quarter
			older.IssueID = "quarter-older"
			older.OriginMS -= quarterMS
			older.IssuedAtMS = older.OriginMS
			older.Lead = LeadBucket(older.OriginMS, older.StartMS)
			hourly := testError("champion", "cfg", "hour", float64(start), float64(start-2*hourMS), 400, 200)
			history = append(history, quarter, older, hourly)
		}
	}
	origin := base + 8*24*hourMS
	unknown := history[0]
	unknown.StartMS = base + 7*24*hourMS
	unknown.EndMS = unknown.StartMS + quarterMS
	unknown.OriginMS = unknown.StartMS - 2*hourMS
	unknown.IssuedAtMS = unknown.OriginMS
	unknown.AvailableAtMS = unknown.EndMS
	unknown.Prediction.StartMS, unknown.Prediction.EndMS = unknown.StartMS, unknown.EndMS
	unknown.PVKnown = false
	unknown.PVErrorW = -900
	unknown.Lead = LeadBucket(unknown.OriginMS, unknown.StartMS)
	late := history[0]
	late.StartMS = base + 7*24*hourMS
	late.EndMS = late.StartMS + quarterMS
	late.OriginMS = late.StartMS - 2*hourMS
	late.IssuedAtMS = late.OriginMS
	late.AvailableAtMS = origin + 1
	late.Prediction.StartMS, late.Prediction.EndMS = late.StartMS, late.EndMS
	late.PVErrorW = -800
	late.Lead = LeadBucket(late.OriginMS, late.StartMS)
	history = append(history, unknown, late)

	calibrator := NewCalibrator(history, "cfg", origin)
	target := origin + 2*hourMS + quarterMS
	quarterBand := calibrator.BandForInterval("champion", "pv", target, target+quarterMS, 1000)
	if quarterBand.Method != BandMethodEmpirical || quarterBand.Samples != 56 || quarterBand.Days != 7 ||
		quarterBand.LowW != 1040 || quarterBand.HighW != 1040 {
		t.Fatalf("quarter-hour calibration = %+v", quarterBand)
	}
	hourBand := calibrator.Band("champion", "pv", target, 1000)
	if hourBand.Method != BandMethodEmpirical || hourBand.Samples != 56 || hourBand.LowW != 1400 || hourBand.HighW != 1400 {
		t.Fatalf("hour calibration mixed with quarter-hour errors: %+v", hourBand)
	}
}

func TestCumulativeNetEnergyUsesOneOriginAndRejectsGaps(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	pointsA := make([]Point, 24)
	pointsZ := make([]Point, 24)
	observations := make([]Observation, 24)
	for i := 0; i < 24; i++ {
		at := start + int64(i)*hourMS
		pointsA[i] = testPoint(at, at+hourMS, 100, 1000)
		pointsZ[i] = testPoint(at, at+hourMS, 150, 1050)
		observations[i] = testObservation("cfg", at, at+hourMS, 200, 1200)
	}
	older := testIssue("a", "cfg", "champion", start-30*hourMS, start-30*hourMS, pointsA)
	later := testIssue("z", "cfg", "champion", start-25*hourMS, start-25*hourMS, pointsZ)
	samples := CumulativeNetEnergy([]Issue{older, later}, observations, start+24*hourMS)
	var full *CumulativeEnergySample
	for i := range samples {
		if samples[i].StartMS == start && samples[i].Hours == 24 {
			full = &samples[i]
			break
		}
	}
	if full == nil || full.IssueID != "z" || full.NetErrorWh != 2400 {
		t.Fatalf("24h matched-origin energy error = %+v", full)
	}
	gapped := append([]Observation(nil), observations[:5]...)
	gapped = append(gapped, observations[6:]...)
	for _, sample := range CumulativeNetEnergy([]Issue{later}, gapped, start+24*hourMS) {
		if sample.StartMS == start && sample.Hours == 24 {
			t.Fatal("gapped truth produced a 24h cumulative error")
		}
	}
	censored := append([]Observation(nil), observations...)
	censored[5].PVKnown = false
	for _, sample := range CumulativeNetEnergy([]Issue{later}, censored, start+24*hourMS) {
		if sample.StartMS == start && sample.Hours == 24 {
			t.Fatal("censored PV truth produced a 24h net error")
		}
	}
}

func TestCompareSeriesUsesOnlyMatchedTruth(t *testing.T) {
	start := float64(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli())
	champion := testError("champion", "cfg", "c", start, start-2*float64(hourMS), 100, 100)
	champion.Prediction.LoadW = 900
	candidate := testError("candidate", "cfg", "n", start, start-2*float64(hourMS), -50, 50)
	candidate.Prediction.PVW = 250
	candidate.Prediction.LoadW = 950
	metrics := CompareSeries([]ErrorSample{champion, candidate}, "champion", "candidate")
	if len(metrics) != 3 {
		t.Fatalf("paired metrics=%+v, want pv/load/net", metrics)
	}
	bySignal := make(map[string]PairMetric)
	for _, metric := range metrics {
		bySignal[metric.Signal] = metric
	}
	if bySignal["pv"].CandidateWinRate != 1 || bySignal["load"].CandidateWinRate != 1 || bySignal["net"].CandidateWinRate != 0 {
		t.Fatalf("unexpected paired win rates: %+v", bySignal)
	}
	mismatch := candidate
	mismatch.Prediction.PVW = 999
	if got := CompareSeries([]ErrorSample{champion, mismatch}, "champion", "candidate"); len(got) != 1 || got[0].Signal != "load" {
		t.Fatalf("mismatched PV/net truth should leave load only, got %+v", got)
	}
}
