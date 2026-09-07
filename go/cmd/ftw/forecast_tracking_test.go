package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/pvmodel"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func trackerNumber(v float64) *float64 { return &v }
func trackerSite() forecastSite {
	return forecastSite{Revision: "site-v1", Meter: "site", Timezone: "Europe/Stockholm", HasLocation: true, HasPVScale: true, Latitude: 59, Longitude: 18}
}
func trackerFixture(at time.Time) *forecastTracker {
	tel := telemetry.NewStore()
	return &forecastTracker{tele: tel, load: loadmodel.NewService(nil, tel, "site", 4000, 0), pv: pvmodel.NewService(nil, tel, func(time.Time) float64 { return 800 }, nil, 8000), site: trackerSite, clock: func() time.Time { return at }, queue: make(chan forecastJob, 8)}
}
func trackerSlots(at time.Time, n int) []mpc.Slot {
	out := make([]mpc.Slot, n)
	for i := range out {
		out[i] = mpc.Slot{StartMs: at.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, PVW: -2000, LoadW: 1000}
	}
	return out
}
func trackerWeather(at, origin time.Time) []state.ForecastPoint {
	return []state.ForecastPoint{{SlotTsMs: at.UnixMilli(), SlotLenMin: 60, FetchedAtMs: origin.Add(-time.Minute).UnixMilli(), Source: "open_meteo", SolarWm2: trackerNumber(500), CloudCoverPct: trackerNumber(10), TempC: trackerNumber(5), PVWEstimated: trackerNumber(4000)}}
}
func TestForecastTrackingKnownPVInputs(t *testing.T) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	site := trackerSite()
	slots := trackerSlots(at, 1)
	tests := []struct {
		name  string
		row   state.ForecastPoint
		site  forecastSite
		known bool
	}{
		{"direct without location", state.ForecastPoint{Source: "forecast_solar", PVWEstimated: trackerNumber(3000)}, forecastSite{}, true},
		{"temp only", state.ForecastPoint{Source: "open_meteo", TempC: trackerNumber(5)}, site, false},
		{"cloud", state.ForecastPoint{Source: "met_no", CloudCoverPct: trackerNumber(80)}, site, true},
		{"invalid cloud", state.ForecastPoint{Source: "met_no", CloudCoverPct: trackerNumber(math.NaN())}, site, false},
		{"unknown scale", state.ForecastPoint{Source: "open_meteo", SolarWm2: trackerNumber(500)}, forecastSite{HasLocation: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.SlotTsMs = at.UnixMilli()
			tc.row.SlotLenMin = 60
			tc.row.FetchedAtMs = at.Add(-time.Minute).UnixMilli()
			p := forecastPoints(slots, []state.ForecastPoint{tc.row}, tc.site, at)[0]
			if p.PVKnown != tc.known {
				t.Fatalf("known=%v want=%v %+v", p.PVKnown, tc.known, p)
			}
		})
	}
	stale := trackerWeather(at, at)
	stale[0].FetchedAtMs = at.Add(-mpc.ForecastMaxAge - time.Second).UnixMilli()
	if forecastPoints(slots, stale, site, at)[0].PVKnown {
		t.Fatal("stale weather marked known")
	}
}
func TestForecastBaselineRequiresWholeHourAndLocalDay(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	target := time.Date(2026, 3, 30, 19, 0, 0, 0, loc)
	prior := time.Date(2026, 3, 23, 19, 0, 0, 0, loc)
	site := trackerSite()
	origin := target.Add(-time.Hour)
	obs := []forecasting.Observation{}
	for i, w := range []float64{1000, 3000, 1000, 3000} {
		start := prior.Add(time.Duration(i) * 15 * time.Minute)
		obs = append(obs, forecasting.Observation{StartMS: start.UnixMilli(), EndMS: start.Add(15 * time.Minute).UnixMilli(), AvailableAtMS: start.Add(15 * time.Minute).UnixMilli(), LoadW: w, PVW: 2000, LoadKnown: true, PVKnown: true, ConfigVersion: site.Revision, Quality: "complete_balance_v1"})
	}
	slots := []mpc.Slot{{StartMs: target.UnixMilli(), LenMin: 60, LoadW: 1000}}
	baselines := forecastBaselines(slots, nil, obs, site, origin)
	week := baselines[2].Points[0]
	if !week.LoadKnown || week.LoadW != 2000 {
		t.Fatalf("DST week/energy wrong: %+v", week)
	}
	broken := forecastBaselines(slots, nil, obs[:3], site, origin)[2].Points[0]
	if broken.LoadKnown || broken.PVKnown {
		t.Fatal("missing quarter stretched into an hour")
	}
	springMissing := time.Date(2026, 3, 30, 2, 0, 0, 0, loc)
	if _, ok := baselineDayShift(springMissing.UnixMilli(), -1, site.Timezone); ok {
		t.Fatal("nonexistent source wall hour accepted")
	}
}
func TestForecastSnapshotFreezesHorizonAndRecord(t *testing.T) {
	origin := time.Date(2026, 1, 5, 11, 59, 0, 0, time.UTC)
	target := origin.Add(time.Minute)
	f := trackerFixture(origin)
	weather := trackerWeather(target, origin)
	in := f.Snapshot(origin, weather)
	before := in.Load(target)
	pvBefore := in.PV(target, 10)
	*weather[0].TempC = -40
	*weather[0].CloudCoverPct = 99
	f.load.SetHeatingCoef(1000)
	f.pv.SetRated(100000)
	if in.Load(target) != before || in.PV(target, 10) != pvBefore {
		t.Fatal("live changes altered captured horizon")
	}
	slots := trackerSlots(target, 1)
	in.Record(slots, slots, "decision", origin.UnixMilli())
	job := <-f.queue
	if err := job.issue.Validate(); err != nil {
		t.Fatal(err)
	}
	slots[0].LoadW = 99999
	if job.issue.Series[0].Points[0].LoadW == 99999 || *job.issue.Weather[0].TempC != 5 {
		t.Fatal("record shared mutable input")
	}
	if job.issue.LatestInputMS != origin.UnixMilli() {
		t.Fatalf("latest frozen occupancy input missing: %d", job.issue.LatestInputMS)
	}
	if job.issue.Series[0].Points[0].LoadQuality != "cold_start" {
		t.Fatal("cold load called trained")
	}
	in.Record(trackerSlots(target, 1), trackerSlots(target, 1), "decision-2", origin.UnixMilli())
	job2 := <-f.queue
	if job.issue.Series[0].ModelVersion != job2.issue.Series[0].ModelVersion {
		t.Fatal("one capture has multiple model revisions")
	}
}

func TestForecastSnapshotRejectsWeatherFromPreviousLocation(t *testing.T) {
	origin := time.Date(2026, 6, 15, 11, 59, 0, 0, time.UTC)
	target := origin.Add(time.Minute)
	f := trackerFixture(origin)
	site := trackerSite()
	site.SiteID, site.LearningRevision = "site", "physical-v2"
	site.WeatherSinceMS = origin.Add(-30 * time.Second).UnixMilli()
	f.site = func() forecastSite { return site }
	weather := trackerWeather(target, origin)
	old := f.Snapshot(origin, weather)
	if len(old.Weather) != 0 {
		t.Fatal("weather captured before a location change survived the snapshot")
	}
	slots := trackerSlots(target, 1)
	old.Record(slots, slots, "old-weather", origin.UnixMilli())
	job := <-f.queue
	if err := job.issue.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(job.issue.Weather) != 0 || job.issue.Series[0].Points[0].PVKnown {
		t.Fatal("old location weather was archived as usable PV evidence")
	}
	if job.issue.Site == nil || job.issue.Site.SiteID != site.SiteID || job.issue.Site.LearningRevision != site.LearningRevision {
		t.Fatal("issue lost the new location binding")
	}
	weather[0].FetchedAtMs = origin.Add(-10 * time.Second).UnixMilli()
	fresh := f.Snapshot(origin, weather)
	if len(fresh.Weather) != 1 {
		t.Fatal("new location weather was rejected")
	}
	fresh.Record(slots, slots, "new-weather", origin.UnixMilli())
	job = <-f.queue
	if len(job.issue.Weather) != 1 || !job.issue.Series[0].Points[0].PVKnown {
		t.Fatal("new location weather did not restore PV evidence")
	}
}

type trackingCandidate struct {
	onSnapshot func()
	snapshot   json.RawMessage
}

func (c *trackingCandidate) Snapshot() json.RawMessage {
	if c.onSnapshot != nil {
		c.onSnapshot()
	}
	return c.snapshot
}
func (c *trackingCandidate) Update(context.Context, forecastSite, forecasting.Observation, *state.ForecastPoint, bool) error {
	return nil
}
func (c *trackingCandidate) Predict(context.Context, forecastSite, forecasting.Issue, json.RawMessage, map[int64]bool) (forecasting.Issue, error) {
	return forecasting.Issue{}, context.Canceled
}
func (c *trackingCandidate) Close() error { return nil }
func TestForecastOriginFollowsCandidateCapture(t *testing.T) {
	at := time.Date(2026, 1, 5, 11, 59, 0, 0, time.UTC)
	f := trackerFixture(at)
	clock := at
	f.clock = func() time.Time { return clock }
	c := &trackingCandidate{snapshot: json.RawMessage("{}"), onSnapshot: func() { clock = at.Add(time.Second) }}
	f.candidate = c
	in := f.Snapshot(at, nil)
	slots := trackerSlots(at.Add(time.Minute), 1)
	in.Record(slots, slots, "decision", clock.UnixMilli())
	job := <-f.queue
	if job.issue.OriginMS != clock.UnixMilli() {
		t.Fatal("origin preceded candidate snapshot")
	}
	c.snapshot[0] = 'x'
	if !json.Valid(job.candidateState) {
		t.Fatal("candidate snapshot aliased")
	}
}
func TestForecastIndependentPVIntervalsAndCurtailment(t *testing.T) {
	f := &forecastTracker{}
	site := trackerSite()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	var out []forecasting.Observation
	for i := 0; i <= 15; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		out = append(out, f.observationIntervals(telemetry.ForecastReading{At: at, Latest: at, PVLatest: at, Valid: false, PVValid: true, PVW: -2000}, site, at)...)
	}
	if len(out) != 1 || !out[0].PVKnown || out[0].LoadKnown || out[0].PVW != 2000 {
		t.Fatalf("missing battery blocked PV: %+v", out)
	}
	f = &forecastTracker{}
	out = nil
	for i := 0; i <= 15; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		out = append(out, f.observationIntervals(telemetry.ForecastReading{At: at, Latest: at, PVLatest: at, Valid: true, PVValid: i != 7, PVW: -2000, HouseholdW: 1000}, site, at)...)
	}
	if len(out) != 1 || out[0].PVKnown || !out[0].LoadKnown {
		t.Fatalf("curtailment became available PV or hid house: %+v", out)
	}
}
func TestForecastMergeDeduplicatesTargetLead(t *testing.T) {
	samples := make(map[forecastErrorKey]forecasting.ErrorSample)
	a := forecasting.ErrorSample{Series: "champion", ConfigVersion: "v1", StartMS: 1000, EndMS: 2000, Lead: 0, OriginMS: 100, IssuedAtMS: 101, IssueID: "a"}
	b := a
	b.OriginMS = 200
	b.IssuedAtMS = 201
	b.IssueID = "b"
	for i := 0; i < 1000; i++ {
		mergeForecastErrors(samples, []forecasting.ErrorSample{b, a})
	}
	if len(samples) != 1 || forecastErrorValues(samples)[0].IssueID != "b" {
		t.Fatal("replans multiplied evidence")
	}
	b.ConfigVersion = "v2"
	mergeForecastErrors(samples, []forecasting.ErrorSample{b})
	if len(samples) != 2 {
		t.Fatal("config epochs merged")
	}
}
func TestForecastArchiveRejectsCurrentPartialAsEvidence(t *testing.T) {
	origin := time.Date(2026, 1, 5, 12, 1, 0, 0, time.UTC)
	f := trackerFixture(origin)
	in := f.Snapshot(origin, trackerWeather(origin.Truncate(time.Hour), origin))
	slots := trackerSlots(origin.Truncate(15*time.Minute), 1)
	in.Record(slots, slots, "decision", origin.UnixMilli())
	job := <-f.queue
	end := slots[0].StartMs + 15*time.Minute.Milliseconds()
	obs := []forecasting.Observation{{StartMS: slots[0].StartMs, EndMS: end, AvailableAtMS: end, PVW: 2000, LoadW: 1000, PVKnown: true, LoadKnown: true, Quality: "complete_balance_v1", ConfigVersion: trackerSite().Revision}}
	if got := forecasting.Errors([]forecasting.Issue{job.issue}, obs, end); len(got) != 0 {
		t.Fatalf("partial issued interval scored: %+v", got)
	}
}
func TestForecastColdPriorRiskPreservesPowerSigns(t *testing.T) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := trackerFixture(at)
	in := f.Snapshot(at, nil)
	base := trackerSlots(at, 2)
	base[1].PVW = 0
	planning := append([]mpc.Slot(nil), base...)
	in.Risk(base, planning, 1)
	if planning[0].PVW <= base[0].PVW || planning[0].PVW > 0 || planning[1].PVW != 0 {
		t.Fatal("cold prior no hedge or invented solar")
	}
	in.Record(base, planning, "decision", at.UnixMilli())
	job := <-f.queue
	if job.issue.Series[0].Points[0].PVBand.Method != forecasting.BandMethodColdStart {
		t.Fatal("cold prior called empirical")
	}
}

func TestForecastBandsUsePointIntervalDuration(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	prior := forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
	history := make([]forecasting.ErrorSample, 0, 56)
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base.Add(time.Duration(day*24+hour) * time.Hour)
			end := start.Add(15 * time.Minute)
			prediction := forecasting.Point{StartMS: start.UnixMilli(), EndMS: end.UnixMilli(), PVW: 100, LoadW: 1000,
				PVKnown: true, LoadKnown: true, PVQuality: "test", LoadQuality: "test", PVBand: prior, LoadBand: prior, NetBand: prior}
			history = append(history, forecasting.ErrorSample{Series: "champion", ConfigVersion: "site-v1", IssueID: "quarter",
				OriginMS: start.Add(-2 * time.Hour).UnixMilli(), IssuedAtMS: start.Add(-2 * time.Hour).UnixMilli(),
				StartMS: start.UnixMilli(), EndMS: end.UnixMilli(), AvailableAtMS: end.UnixMilli(),
				Lead: forecasting.LeadBucket(start.Add(-2*time.Hour).UnixMilli(), start.UnixMilli()), PVErrorW: 40, LoadErrorW: 20,
				PVKnown: true, LoadKnown: true, Prediction: prediction})
		}
	}
	origin := base.Add(8 * 24 * time.Hour)
	target := origin.Add(2*time.Hour + 15*time.Minute)
	issue := forecasting.Issue{Series: []forecasting.Series{{Name: "champion", Points: []forecasting.Point{{
		StartMS: target.UnixMilli(), EndMS: target.Add(15 * time.Minute).UnixMilli(), PVW: 1000, LoadW: 2000,
	}}}}}
	applyForecastBandsWith(&issue, forecasting.NewCalibrator(history, "site-v1", origin.UnixMilli()))
	band := issue.Series[0].Points[0].PVBand
	if band.Method != forecasting.BandMethodEmpirical || band.Samples != 56 || band.LowW != 1040 || band.HighW != 1040 {
		t.Fatalf("quarter-hour point band = %+v", band)
	}
}

func TestForecastStopDrainsIssuedChampion(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UTC()
	f := trackerFixture(at)
	f.store = st
	// Prevent the worker from running until a queued issue exists, then cancel
	// immediately. Both queued and in-flight persistence must survive Stop.
	if err := st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	in := f.Snapshot(at, trackerWeather(at.Add(time.Minute), at))
	slots := trackerSlots(at.Add(time.Minute), 1)
	in.Record(slots, slots, "shutdown-decision", at.UnixMilli())
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.wg = sync.WaitGroup{}
	f.wg.Add(1)
	cancel()
	go func() { defer f.wg.Done(); f.run(ctx) }()
	f.Stop()
	issues, err := st.LoadForecastIssues(context.Background(), at.Add(-time.Hour).UnixMilli(), at.Add(time.Hour).UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("shutdown lost issued forecast: %d", len(issues))
	}
}
func TestForecastFullHorizonRecordFitsArchive(t *testing.T) {
	at := time.Now().UTC().Truncate(15 * time.Minute)
	f := trackerFixture(at)
	rows := []state.ForecastPoint{}
	for h := 0; h < 48; h++ {
		rows = append(rows, trackerWeather(at.Add(time.Duration(h)*time.Hour), at)...)
	}
	in := f.Snapshot(at, rows)
	slots := trackerSlots(at, 192)
	for i := range slots {
		slots[i].LoadW = 1234.567 + float64(i)*0.03
		slots[i].PVW = -2345.678
	}
	in.Record(slots, slots, "full-horizon", at.UnixMilli())
	job := <-f.queue
	if err := job.issue.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveForecastIssue(context.Background(), job.issue); err != nil {
		raw, _ := json.Marshal(job.issue)
		t.Fatalf("full horizon archive rejected (%d embedded bytes): %v", len(raw), err)
	}
	restored, err := st.LoadForecastIssues(context.Background(), at.Add(-time.Hour).UnixMilli(), at.Add(time.Hour).UnixMilli(), 1)
	if err != nil || len(restored) != 1 {
		t.Fatalf("full horizon unavailable: %v", err)
	}
	raw, _ := json.Marshal(restored[0])
	t.Logf("stored full horizon JSON bytes=%d", len(raw))
}
func TestForecastConfigChangeBreaksInterval(t *testing.T) {
	f := &forecastTracker{}
	site := trackerSite()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 15; i++ {
		if i == 8 {
			site.Revision = "changed"
		}
		at := start.Add(time.Duration(i) * time.Minute)
		out := f.observationIntervals(telemetry.ForecastReading{At: at, Latest: at, PVLatest: at, Valid: true, PVValid: true, HouseholdW: 1000, PVW: -2000}, site, at)
		if len(out) > 0 {
			t.Fatal("interval crosses config revision")
		}
	}
}
func TestForecastMalformedWeatherDoesNotPoisonArchive(t *testing.T) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := trackerFixture(at)
	rows := trackerWeather(at, at)
	rows[0].SolarWm2 = trackerNumber(math.NaN())
	rows[0].CloudCoverPct = nil
	in := f.Snapshot(at, rows)
	slots := trackerSlots(at, 1)
	in.Record(slots, slots, "bad-weather", at.UnixMilli())
	job := <-f.queue
	if err := job.issue.Validate(); err != nil {
		t.Fatal(err)
	}
	if job.issue.Series[0].Points[0].PVKnown {
		t.Fatal("malformed weather marked as usable PV")
	}
}

func TestForecastDirectPVAndStableModelStateArchive(t *testing.T) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := trackerFixture(at)
	rows := trackerWeather(at, at)
	rows[0].Source = "forecast_solar"
	slots := trackerSlots(at, 1)
	f.Snapshot(at, rows).Record(slots, slots, "first", at.UnixMilli())
	first := <-f.queue
	f.Snapshot(at, rows).Record(slots, slots, "second", at.UnixMilli())
	second := <-f.queue
	if first.issue.Weather[0].DirectPVW == nil || *first.issue.Weather[0].DirectPVW != 4000 {
		t.Fatal("direct forecast power missing from input archive")
	}
	if string(first.issue.Models[0].State) != string(second.issue.Models[0].State) {
		t.Fatal("unchanged model state differs merely because capture time changed")
	}
}

func BenchmarkForecastTrackingFullHorizon(b *testing.B) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := trackerFixture(at)
	rows := []state.ForecastPoint{}
	for h := 0; h < 48; h++ {
		rows = append(rows, trackerWeather(at.Add(time.Duration(h)*time.Hour), at)...)
	}
	slots := trackerSlots(at, 192)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := f.Snapshot(at, rows)
		for _, s := range slots {
			_ = in.Load(time.UnixMilli(s.StartMs))
		}
		in.Risk(slots, append([]mpc.Slot(nil), slots...), 1)
		in.Record(slots, slots, "benchmark", at.UnixMilli())
		<-f.queue
	}
}
