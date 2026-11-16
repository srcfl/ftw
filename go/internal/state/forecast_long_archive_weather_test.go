package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func longWeather(source string, start, avail int64) forecasting.Weather {
	ghi := float64(start%100000) + float64(avail%1000)/10
	cloud := float64(avail % 100)
	temp := -3.5
	return forecasting.Weather{
		StartMS: start, EndMS: start + time.Hour.Milliseconds(), AvailableAtMS: avail,
		Source: source, GHIWm2: &ghi, CloudPct: &cloud, TempC: &temp,
	}
}

func countRows(t *testing.T, arch *ForecastLongArchive, query string, args ...any) int {
	t.Helper()
	var n int
	if err := arch.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A partial weather refresh leaves one issue with rows from two fetches.
// Another run that used the older fetch for the same slot must not bring it
// back into this one.
func TestForecastLongMixedFetchWeatherRoundTrip(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	origin := longOrigin()
	s := origin.UnixMilli()
	hour := time.Hour.Milliseconds()
	fetchA := s - hour
	fetchB := s + 10*time.Minute.Milliseconds()

	first := longIssue("first", origin)
	first.Weather = []forecasting.Weather{
		longWeather("open_meteo", s, fetchA), longWeather("open_meteo", s+hour, fetchA), longWeather("open_meteo", s+2*hour, fetchA),
	}
	second := longIssue("second", origin.Add(15*time.Minute))
	second.Weather = []forecasting.Weather{
		longWeather("open_meteo", s, fetchB), longWeather("open_meteo", s+hour, fetchA), longWeather("open_meteo", s+2*hour, fetchA),
	}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: first}, {Issue: second}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_weather`); n != 4 {
		t.Fatalf("weather rows = %d, want 4 shared rows", n)
	}
	for _, want := range []forecasting.Issue{first, second} {
		got, err := arch.LoadIssue(ctx, want.ID)
		if err != nil {
			t.Fatalf("%s: %v", want.ID, err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("%s: %v", want.ID, err)
		}
		if !reflect.DeepEqual(got.Weather, want.Weather) {
			t.Fatalf("%s weather = %+v, want %+v", want.ID, got.Weather, want.Weather)
		}
	}
	// The same content again is a no-op; a changed weather row is refused.
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: second}}); err != nil {
		t.Fatalf("identical re-archive: %v", err)
	}
	changed := second
	changed.Weather = append([]forecasting.Weather(nil), second.Weather...)
	changed.Weather[0] = longWeather("open_meteo", s, fetchA)
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: changed}}); !errors.Is(err, errForecastArchiveImmutable) {
		t.Fatalf("changed weather err = %v, want immutable", err)
	}
	got, err := arch.LoadIssue(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Weather, second.Weather) {
		t.Fatalf("stored weather changed to %+v", got.Weather)
	}
}

// Every issued weather row comes back, including rows before the origin and
// past the last series point.
func TestForecastLongWeatherOutsideSeriesSpanRoundTrips(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	origin := longOrigin()
	s := origin.UnixMilli()
	hour := time.Hour.Milliseconds()
	issue := longIssue("span", origin)
	issue.Weather = []forecasting.Weather{
		longWeather("open_meteo", s-3*hour, s-4*hour),
		longWeather("open_meteo", s, s-hour),
		longWeather("forecast_solar", s+hour, s-2*hour),
		longWeather("open_meteo", s+30*hour, s-hour),
	}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	got, err := arch.LoadIssue(ctx, "span")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Weather, issue.Weather) {
		t.Fatalf("weather = %+v, want %+v", got.Weather, issue.Weather)
	}
}

// The plans are checked, not timed: a scan of a reference table inside the
// correlated subquery is what made garbage collection quadratic.
func TestForecastLongGarbagePlansUseIndexes(t *testing.T) {
	_, arch := openLongArchive(t)
	assertPlanSearches(t, arch, forecastLongWeatherGC, []any{forecastPruneBatch}, "fa_run_weather", "fa_run_weather_fetch")
	assertPlanSearches(t, arch, forecastLongStateGC, []any{forecastPruneBatch}, "fa_run_states", "fa_run_states_ref")
}

// assertPlanSearches fails unless the query plan reads table through index
// and never scans it.
func assertPlanSearches(t *testing.T, arch *ForecastLongArchive, query string, args []any, table, index string) {
	t.Helper()
	rows, err := arch.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	searched := false
	for _, detail := range plan {
		if strings.HasPrefix(detail, "SCAN "+table) {
			t.Fatalf("%s is scanned: %q", table, plan)
		}
		if strings.HasPrefix(detail, "SEARCH "+table+" ") && strings.Contains(detail, "INDEX "+index+" ") {
			searched = true
		}
	}
	if !searched {
		t.Fatalf("%s is not searched through %s: %q", table, index, plan)
	}
}

func TestForecastLongGarbageKeepsLinkedWeather(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	origin := longOrigin()
	s := origin.UnixMilli()
	hour := time.Hour.Milliseconds()
	fetchA, fetchB, fetchC := s-3*hour, s-2*hour, s-hour

	only := longIssue("only-a", origin)
	only.Weather = []forecasting.Weather{longWeather("open_meteo", s, fetchA), longWeather("open_meteo", s+hour, fetchA)}
	mixed := longIssue("mixed", origin.Add(15*time.Minute))
	mixed.Weather = []forecasting.Weather{longWeather("open_meteo", s, fetchB), longWeather("open_meteo", s+hour, fetchC)}
	shared := longIssue("shared", origin.Add(30*time.Minute))
	shared.Weather = []forecasting.Weather{longWeather("open_meteo", s, fetchB), longWeather("open_meteo", s+2*hour, fetchC)}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: only}, {Issue: mixed}, {Issue: shared}}); err != nil {
		t.Fatal(err)
	}
	if err := arch.deleteRunIDs(ctx, []string{"only-a", "mixed"}); err != nil {
		t.Fatal(err)
	}
	if err := arch.collectGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_weather WHERE available_at_ms=?`, fetchA); n != 0 {
		t.Fatalf("orphan fetch rows = %d, want 0", n)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_run_weather`); n != 2 {
		t.Fatalf("weather links = %d, want the kept run's two fetches", n)
	}
	got, err := arch.LoadIssue(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Weather, shared.Weather) {
		t.Fatalf("kept weather = %+v, want %+v", got.Weather, shared.Weather)
	}
	if err := arch.deleteRunIDs(ctx, []string{"shared"}); err != nil {
		t.Fatal(err)
	}
	if err := arch.collectGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_weather`); n != 0 {
		t.Fatalf("weather rows after the last run = %d, want 0", n)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_model_states`); n != 0 {
		t.Fatalf("model states after the last run = %d, want 0", n)
	}
}

func TestForecastLongWeatherStartsCodec(t *testing.T) {
	hour := time.Hour.Milliseconds()
	s := longOrigin().UnixMilli()
	starts := []int64{s + 2*hour, s, s + hour, s + 47*hour}
	blob, err := encodeForecastStarts(starts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeForecastStarts(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int64{s, s + hour, s + 2*hour, s + 47*hour}) {
		t.Fatalf("starts = %v", got)
	}
	day := make([]int64, 48)
	for i := range day {
		day[i] = s + int64(i)*hour
	}
	if blob, err = encodeForecastStarts(day); err != nil || len(blob) > 200 {
		t.Fatalf("hourly day = %d bytes err=%v", len(blob), err)
	}
	for _, bad := range [][]int64{nil, {s, s}, {0}} {
		if _, err := encodeForecastStarts(bad); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	for _, bad := range [][]byte{nil, {0x80}, {0x01, 0x00}} {
		if _, err := decodeForecastStarts(bad); err == nil {
			t.Fatalf("%x was accepted", bad)
		}
	}
}

// A run must load with every weather row it was issued with. If one linked
// row is gone, LoadIssue fails rather than return a shorter list.
func TestForecastLongLoadFailsWhenLinkedWeatherIsMissing(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	origin := longOrigin()
	s := origin.UnixMilli()
	hour := time.Hour.Milliseconds()
	issue := longIssue("gap", origin)
	issue.Weather = []forecasting.Weather{longWeather("open_meteo", s, s-hour), longWeather("open_meteo", s+hour, s-hour)}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `DELETE FROM fa_weather WHERE start_ms=?`, s+hour); err != nil {
		t.Fatal(err)
	}
	got, err := arch.LoadIssue(ctx, "gap")
	if err == nil || !strings.Contains(err.Error(), "missing 1 weather rows") {
		t.Fatalf("LoadIssue = %d weather rows, err=%v; want a missing-row error", len(got.Weather), err)
	}
}
