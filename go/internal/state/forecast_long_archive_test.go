package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func openLongArchive(t *testing.T) (*Store, *ForecastLongArchive) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	arch, err := store.OpenForecastLongArchive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return store, arch
}

func longOrigin() time.Time {
	return time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
}

func longBand(low, high float64, method string) forecasting.Band {
	b := forecasting.Band{LowW: low, HighW: high, Method: method}
	if method == forecasting.BandMethodEmpirical {
		b.Samples, b.Days = 48, 7
	}
	return b
}

func longPoint(start int64, pv, load float64, partial bool) forecasting.Point {
	p := forecasting.Point{
		StartMS: start, EndMS: start + 15*60*1000, PVW: pv, LoadW: load,
		PVKnown: true, LoadKnown: load >= 0, PVQuality: "ready", LoadQuality: "learning",
		PVSource: "energyplan", LoadSource: "legacy",
		PVBand:   longBand(0, pv+10, forecasting.BandMethodEmpirical),
		LoadBand: longBand(0, 0, forecasting.BandMethodColdStart),
		NetBand:  longBand(0, 0, forecasting.BandMethodColdStart),
	}
	if partial {
		p.PredictionStartMS = start + 60*1000
	}
	return p
}

func longIssue(id string, origin time.Time) forecasting.Issue {
	start := origin.UnixMilli()
	ghi := 120.5
	temp := 4.25
	quarter := int64(15 * 60 * 1000)
	return forecasting.Issue{
		Schema: forecasting.Schema, ID: id, DecisionID: "decision-" + id,
		OriginMS: start, IssuedAtMS: start + 1000, LatestInputMS: start,
		ConfigVersion: "cfg-v1",
		Site: &forecasting.SiteContext{
			SiteID: "site-1", LearningRevision: "learn-1", Timezone: "UTC",
			HasLocation: true, Latitude: 59.3293, Longitude: 18.0686,
		},
		Models: []forecasting.ModelState{{
			Name: "load", Version: "load-v3", Quality: forecasting.ModelQualityWarm,
			UpdatedAtMS: start, State: []byte(`{"weights":[1,2,3]}`),
		}},
		Weather: []forecasting.Weather{{
			StartMS: start, EndMS: start + quarter, AvailableAtMS: start,
			Source: "open_meteo", GHIWm2: &ghi, TempC: &temp,
		}},
		Occupancy: []forecasting.Occupancy{
			{StartMS: start - 2*quarter, EndMS: start - quarter, AvailableAtMS: start - quarter, Home: true},
			{StartMS: start - quarter, EndMS: start, AvailableAtMS: start, Home: false},
		},
		Series: []forecasting.Series{
			{Name: "champion", ModelVersion: "champ-1", Points: []forecasting.Point{longPoint(start, 1500.5, 800.25, true)}},
			{Name: "planning", ModelVersion: "plan-1", Points: []forecasting.Point{longPoint(start, 1400, 900, false)}},
			{Name: "legacy_shadow", ModelVersion: "legacy-1", Points: []forecasting.Point{longPoint(start, 1000, 700, false)}},
			{Name: "energyplan", ModelVersion: "ep-1", Points: []forecasting.Point{
				longPoint(start, 1500.5, 800.25, false),
				func() forecasting.Point {
					p := longPoint(start+30*60*1000, 400, 650, false)
					p.EndMS = start + 90*60*1000
					return p
				}(),
			}},
			{Name: "previous_day", ModelVersion: "baseline", Points: []forecasting.Point{longPoint(start, 1, 1, false)}},
		},
	}
}

func TestForecastLongArchiveOpensAndReopens(t *testing.T) {
	store, arch := openLongArchive(t)
	var marker string
	if err := arch.db.QueryRow(`SELECT name FROM fa_migrations`).Scan(&marker); err != nil || marker != "fa-v1" {
		t.Fatalf("migration = %q err=%v", marker, err)
	}
	var mode int
	if err := arch.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d err=%v, want incremental", mode, err)
	}
	if err := arch.ArchiveIssues(context.Background(), []ForecastArchiveIssue{{Issue: longIssue("one", longOrigin())}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	again, err := Open(store.mainDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	arch, err = again.OpenForecastLongArchive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migrations = %d err=%v", n, err)
	}
	if _, err := arch.LoadIssue(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
}

func TestForecastLongArchiveRoundTrip(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	issue := longIssue("round", longOrigin())
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue, JSONBytes: 1200, GzipBytes: 400}}); err != nil {
		t.Fatal(err)
	}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue, JSONBytes: 1200, GzipBytes: 400}}); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs = %d err=%v", runs, err)
	}
	loaded, err := arch.LoadIssue(ctx, "round")
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
	byName := map[string]forecasting.Series{}
	for _, series := range loaded.Series {
		byName[series.Name] = series
	}
	if len(byName) != 4 {
		t.Fatalf("stored series = %d, baselines should be omitted", len(byName))
	}
	if _, ok := byName["previous_day"]; ok {
		t.Fatal("baseline series was stored")
	}
	want := map[string]forecasting.Series{}
	for _, series := range issue.Series {
		if forecastLongKeepSeries(series.Name) {
			want[series.Name] = series
		}
	}
	for name, series := range want {
		got := byName[name]
		if got.ModelVersion != series.ModelVersion || len(got.Points) != len(series.Points) {
			t.Fatalf("%s identity = %+v", name, got)
		}
		for i, point := range series.Points {
			gp := got.Points[i]
			if float32(gp.PVW) != float32(point.PVW) || float32(gp.LoadW) != float32(point.LoadW) ||
				gp.PVKnown != point.PVKnown || gp.LoadKnown != point.LoadKnown ||
				gp.PVQuality != point.PVQuality || gp.LoadQuality != point.LoadQuality ||
				gp.PVSource != point.PVSource || gp.LoadSource != point.LoadSource ||
				gp.StartMS != point.StartMS || gp.EndMS != point.EndMS {
				t.Fatalf("%s point %d = %+v want %+v", name, i, gp, point)
			}
		}
	}
	champ := byName["champion"].Points[0]
	if champ.PredictionStartMS != issue.Series[0].Points[0].PredictionStartMS ||
		champ.PVBand.Method != forecasting.BandMethodEmpirical || champ.PVBand.Samples != 48 || champ.PVBand.Days != 7 ||
		float32(champ.PVBand.HighW) != float32(issue.Series[0].Points[0].PVBand.HighW) {
		t.Fatalf("champion band = %+v", champ)
	}
	if byName["planning"].Points[0].PVBand.Method != forecasting.BandMethodColdStart {
		t.Fatal("non-champion band was stored")
	}
	if loaded.Site == nil || loaded.Site.SiteID != "site-1" || loaded.Site.Latitude != issue.Site.Latitude {
		t.Fatalf("site = %+v", loaded.Site)
	}
	if len(loaded.Occupancy) != 2 || loaded.Occupancy[0].Home != true || loaded.Occupancy[1].Home != false ||
		loaded.Occupancy[0].AvailableAtMS != issue.Occupancy[0].AvailableAtMS {
		t.Fatalf("occupancy = %+v", loaded.Occupancy)
	}
	if len(loaded.Weather) != 1 || loaded.Weather[0].GHIWm2 == nil || *loaded.Weather[0].GHIWm2 != *issue.Weather[0].GHIWm2 ||
		loaded.Weather[0].CloudPct != nil {
		t.Fatalf("weather = %+v", loaded.Weather)
	}
	sum := sha256.Sum256(issue.Models[0].State)
	if len(loaded.Models) != 1 || loaded.Models[0].StateID != hex.EncodeToString(sum[:]) || len(loaded.Models[0].State) != 0 ||
		loaded.Models[0].Version != "load-v3" {
		t.Fatalf("models = %+v", loaded.Models)
	}
	var jsonBytes, gzipBytes int
	if err := arch.db.QueryRow(`SELECT json_bytes, gzip_bytes FROM fa_runs WHERE run_id='round'`).Scan(&jsonBytes, &gzipBytes); err != nil || jsonBytes != 1200 || gzipBytes != 400 {
		t.Fatalf("measured sizes %d %d err=%v", jsonBytes, gzipBytes, err)
	}
}

func TestForecastLongArchiveRejectsChangedIdentity(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	issue := longIssue("same", longOrigin())
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	changed := issue
	changed.Series = append([]forecasting.Series(nil), issue.Series...)
	changed.Series[0].Points = append([]forecasting.Point(nil), issue.Series[0].Points...)
	changed.Series[0].Points[0].LoadW = 42
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: changed}}); err == nil {
		t.Fatal("changed payload was accepted")
	}
	loaded, err := arch.LoadIssue(ctx, "same")
	if err != nil {
		t.Fatal(err)
	}
	var champ forecasting.Point
	for _, series := range loaded.Series {
		if series.Name == "champion" {
			champ = series.Points[0]
		}
	}
	if float32(champ.LoadW) != float32(issue.Series[0].Points[0].LoadW) {
		t.Fatalf("stored load changed to %v", champ.LoadW)
	}
}

func TestForecastLongArchiveDeduplicatesWeather(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	first := longIssue("a", longOrigin())
	second := longIssue("b", longOrigin().Add(time.Hour))
	second.Weather = first.Weather
	second.Weather[0].AvailableAtMS = first.OriginMS
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: first}, {Issue: second}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_weather`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("weather rows = %d err=%v", n, err)
	}
}

func TestForecastLongEncodedHorizonSize(t *testing.T) {
	_, arch := openLongArchive(t)
	origin := longOrigin()
	issue := longIssue("horizon", origin)
	quarter := int64(15 * 60 * 1000)
	points := make([]forecasting.Point, forecasting.MaxPoints)
	for i := range points {
		points[i] = longPoint(origin.UnixMilli()+int64(i)*quarter, float64(100+i), float64(500+i), i == 0)
	}
	for i := range issue.Series {
		if forecastLongKeepSeries(issue.Series[i].Name) {
			issue.Series[i].Points = points
		}
	}
	// The last baseline series is not stored. Drop the irregular second point
	// by replacing every kept series with the regular horizon.
	if err := arch.ArchiveIssues(context.Background(), []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	var blobs int
	if err := arch.db.QueryRow(`SELECT COALESCE(SUM(length(pv_w)+length(load_w)+COALESCE(length(pv_lo),0)+COALESCE(length(pv_hi),0)),0) FROM fa_series`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	// Four series of 193 float32 pairs, plus champion bands. This is a fixture
	// measurement, not a box measurement.
	t.Logf("encoded power and champion band bytes=%d", blobs)
	if blobs < 4000 || blobs > 20000 {
		t.Fatalf("encoded horizon = %d bytes, outside the expected fixture range", blobs)
	}
	if math.IsNaN(float64(blobs)) {
		t.Fatal("size was not a number")
	}
}

func TestForecastLongGappedOccupancy(t *testing.T) {
	_, arch := openLongArchive(t)
	issue := longIssue("gap", longOrigin())
	quarter := int64(15 * 60 * 1000)
	start := issue.OriginMS
	issue.Occupancy = []forecasting.Occupancy{
		{StartMS: start - 4*quarter, EndMS: start - 3*quarter, AvailableAtMS: start - 3*quarter, Home: true},
		{StartMS: start - quarter, EndMS: start, AvailableAtMS: start, Home: false},
	}
	if err := arch.ArchiveIssues(context.Background(), []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := arch.LoadIssue(context.Background(), "gap")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Occupancy) != 2 || loaded.Occupancy[0].StartMS != issue.Occupancy[0].StartMS || loaded.Occupancy[1].Home {
		t.Fatalf("gapped occupancy = %+v", loaded.Occupancy)
	}
}
