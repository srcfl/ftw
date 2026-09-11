package forecasting

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func testSiteContext() *SiteContext {
	return &SiteContext{SiteID: "site", LearningRevision: "physical-v1", Timezone: "Europe/Stockholm", HasLocation: true, Latitude: 59.3, Longitude: 18.1}
}

func TestIssueSiteContextAndEstimatedPVSurviveJSON(t *testing.T) {
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	issue := testIssue("i", "c", "champion", start-hourMS, start-hourMS, []Point{testPoint(start, start+hourMS, 1234, 1000)})
	issue.Site = testSiteContext()
	estimated := 1234.0
	issue.Weather = []Weather{{StartMS: start, EndMS: start + hourMS, AvailableAtMS: issue.OriginMS, Source: "open_meteo", EstimatedPVW: &estimated}}
	if err := issue.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Issue
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Site == nil || *decoded.Site != *issue.Site || decoded.Weather[0].EstimatedPVW == nil || *decoded.Weather[0].EstimatedPVW != estimated {
		t.Fatal("replay inputs lost")
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		issue.Weather[0].EstimatedPVW = &bad
		if issue.Validate() == nil {
			t.Fatal("invalid estimated PV accepted")
		}
	}
	issue.Site = nil
	issue.Weather = nil
	if issue.Validate() != nil {
		t.Fatal("legacy issue without site context rejected")
	}
}

func TestSiteContextRequiresReplayableIdentityClockAndCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SiteContext)
	}{
		{"empty site", func(s *SiteContext) { s.SiteID = "" }},
		{"oversize revision", func(s *SiteContext) { s.LearningRevision = strings.Repeat("x", 129) }},
		{"host clock", func(s *SiteContext) { s.Timezone = "Local" }},
		{"missing zone", func(s *SiteContext) { s.Timezone = "" }},
		{"unknown zone", func(s *SiteContext) { s.Timezone = "unknown/place" }},
		{"latitude", func(s *SiteContext) { s.Latitude = 91 }},
		{"longitude", func(s *SiteContext) { s.Longitude = -181 }},
		{"nonfinite unused coordinate", func(s *SiteContext) { s.HasLocation = false; s.Latitude = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSiteContext()
			tc.mutate(s)
			if s.Validate() == nil {
				t.Fatal("invalid replay context accepted")
			}
		})
	}
	s := testSiteContext()
	s.Timezone = "UTC"
	s.HasLocation = false
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDaylightMetricsIncludeZeroCandidateAndCountIndependentDays(t *testing.T) {
	var issues []Issue
	var observations []Observation
	first := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	for day := 0; day < 2; day++ {
		for _, hour := range []int{0, 10} {
			start := first.AddDate(0, 0, day).Add(time.Duration(hour) * time.Hour).UnixMilli()
			actual := 0.0
			if hour == 10 {
				actual = 1000
			}
			for _, name := range []string{"champion", "candidate"} {
				prediction := 0.0
				if name == "champion" && hour == 10 {
					prediction = 900
				}
				issue := testIssue(name+time.UnixMilli(start).String(), "cfg", name, start-hourMS, start-hourMS, []Point{testPoint(start, start+hourMS, prediction, 1000)})
				issue.Site = testSiteContext()
				issues = append(issues, issue)
			}
			observations = append(observations, testObservation("cfg", start, start+hourMS, actual, 1000))
		}
	}
	samples := Errors(issues, observations, first.Add(48*time.Hour).UnixMilli())
	if len(samples) != 8 {
		t.Fatalf("scored %d intervals", len(samples))
	}
	var candidateDay, candidateAll Metric
	for _, m := range Metrics(samples) {
		if m.Series == "candidate" {
			if m.Signal == "pv_daylight" {
				candidateDay = m
			}
			if m.Signal == "pv" {
				candidateAll = m
			}
		}
	}
	if candidateDay.Samples != 2 || candidateDay.Days != 2 || candidateDay.MAEW != 1000 {
		t.Fatalf("daylight candidate-zero evaluation=%+v", candidateDay)
	}
	if candidateAll.Samples != 4 || candidateAll.Days != 2 || candidateAll.MAEW != 500 {
		t.Fatalf("all-hours PV evaluation changed=%+v", candidateAll)
	}
	found := false
	for _, m := range CompareSeries(samples, "champion", "candidate") {
		if m.Signal == "pv_daylight" {
			found = true
			if m.CandidateWinRate != 0 || m.Samples != 2 {
				t.Fatalf("daylight comparison=%+v", m)
			}
		}
	}
	if !found {
		t.Fatal("daylight comparison absent")
	}
}

func TestDaylightUsesIndependentSunOrWeatherSignal(t *testing.T) {
	start := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	ghi, pv := 500.0, 9000.0
	issue := Issue{Site: testSiteContext(), Weather: []Weather{{StartMS: start, EndMS: start + hourMS, GHIWm2: &ghi, DirectPVW: &pv, EstimatedPVW: &pv}}}
	if intervalDaylight(issue, start, start+hourMS) {
		t.Fatal("nighttime irradiance or PV prediction overrode sun position")
	}
	issue.Site = nil
	if !intervalDaylight(issue, start, start+hourMS) {
		t.Fatal("legacy irradiance evidence ignored")
	}
	ghi = 20
	if intervalDaylight(issue, start, start+hourMS) {
		t.Fatal("GHI threshold is not strict")
	}
	issue.Weather[0].GHIWm2 = nil
	if intervalDaylight(issue, start, start+hourMS) {
		t.Fatal("forecast PV selected its own evaluation subset")
	}
}

func TestDaylightSubsetDoesNotChangeDefaultCalibration(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	var history []ErrorSample
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base + int64(day*24+hour)*hourMS
			history = append(history, testError("champion", "cfg", "i", float64(start), float64(start-2*hourMS), float64(day*10+hour), 0))
		}
	}
	origin := base + 8*24*hourMS
	before := NewCalibrator(history, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 1000)
	for i := range history {
		history[i].Daylight = true
	}
	after := NewCalibrator(history, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 1000)
	if before != after || before.Method != BandMethodEmpirical || before.Samples != 56 {
		t.Fatalf("daylight annotation changed calibration: before=%+v after=%+v", before, after)
	}
}
