package forecast

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Stub server returns a canned Forecast.Solar response. Verifies that
// per-period mean watts get split into energy-preserving UTC hours and
// materialised on PVWEstimated.
func TestForecastSolar_SunriseIntervalsPreserveEnergy(t *testing.T) {
	body := `{
		"result": {
			"watts": {
				"2026-04-17T06:30:00+00:00": 0.0,
				"2026-04-17T07:00:00+00:00": 600.0,
				"2026-04-17T07:30:00+00:00": 1200.0,
				"2026-04-17T08:00:00+00:00": 0.0
			}
		},
		"message": {"code": 0, "type": "success"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify geometry lands in the URL.
		if !strings.Contains(r.URL.Path, "/estimate/56.7") {
			t.Errorf("missing lat in path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "time=utc") {
			t.Errorf("expected time=utc, got %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	fs := &ForecastSolarProvider{
		Client: srv.Client(), BaseURL: srv.URL,
		Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}},
	}
	rows, err := fs.Fetch(context.Background(), 56.7, 16.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want hour buckets 06 and 07, got %d", len(rows))
	}
	if rows[0].HourStart.Hour() != 6 || rows[0].PVWEstimated == nil || *rows[0].PVWEstimated != 300 {
		t.Errorf("06:00 bucket = %+v, want 300 W (300 Wh)", rows[0])
	}
	if rows[1].HourStart.Hour() != 7 || rows[1].PVWEstimated == nil || *rows[1].PVWEstimated != 600 {
		t.Errorf("07:00 bucket = %+v, want 600 W (600 Wh)", rows[1])
	}
	// No fabrication of cloud/temp.
	for _, r := range rows {
		if r.CloudCoverPct != nil {
			t.Error("forecast.solar shouldn't emit CloudCoverPct")
		}
		if r.TempC != nil {
			t.Error("forecast.solar shouldn't emit TempC")
		}
	}
	// Sorted by time.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].HourStart.After(rows[i].HourStart) {
			t.Errorf("rows not sorted: %s > %s", rows[i-1].HourStart, rows[i].HourStart)
		}
	}
}

// 429 rate-limit surfaces with a clear error — not a silent zero-row
// response that would look like "no sun ever".
func TestForecastSolar_RateLimitedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Reset", "3600")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"message":{"code":429,"type":"error","text":"rate limit"}}`)
	}))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}}
	_, err := fs.Fetch(context.Background(), 0, 0)
	if err == nil || !strings.Contains(err.Error(), "rate") {
		t.Errorf("want rate-limit error, got %v", err)
	}
}

// Zero or negative kWp is a config mistake — fail fast instead of
// emitting a nonsense URL.
func TestForecastSolar_RejectsZeroKWp(t *testing.T) {
	fs := NewForecastSolar(35, 180, 0)
	if _, err := fs.Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error for kWp=0, got nil")
	}
}

func TestForecastSolar_RejectsInvalidGeometry(t *testing.T) {
	for name, array := range map[string]Array{
		"negative tilt": {TiltDeg: -1, AzimuthDeg: 180, RatedW: 1000},
		"large tilt":    {TiltDeg: 91, AzimuthDeg: 180, RatedW: 1000},
		"negative az":   {TiltDeg: 30, AzimuthDeg: -1, RatedW: 1000},
		"large az":      {TiltDeg: 30, AzimuthDeg: 361, RatedW: 1000},
		"non-finite":    {TiltDeg: 30, AzimuthDeg: 180, RatedW: math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			fs := NewForecastSolarMulti([]Array{array})
			if _, err := fs.Fetch(context.Background(), 0, 0); err == nil {
				t.Fatal("expected geometry error")
			}
		})
	}
}

func TestForecastSolar_ConvertsCompassAzimuth(t *testing.T) {
	for name, tc := range map[string]struct {
		compass float64
		vendor  string
	}{
		"north": {compass: 0, vendor: "-180.0"},
		"east":  {compass: 90, vendor: "-90.0"},
		"south": {compass: 180, vendor: "0.0"},
		"west":  {compass: 270, vendor: "90.0"},
	} {
		t.Run(name, func(t *testing.T) {
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				fmt.Fprint(w, `{"result":{"watts":{}}}`)
			}))
			defer srv.Close()
			fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: tc.compass, RatedW: 10000}}}
			if _, err := fs.Fetch(context.Background(), 0, 0); err != nil {
				t.Fatal(err)
			}
			if want := "/35.0/" + tc.vendor + "/10.00"; !strings.HasSuffix(path, want) {
				t.Errorf("path=%q, want suffix %q", path, want)
			}
		})
	}
}

// Multi-plane URL: two arrays → URL has two (tilt/azimuth/kwp)
// triplets back-to-back, same syntax forecast.solar documents. Verifies
// the URL path is constructed correctly when the site has more than
// one roof plane (e.g. south + east).
func TestForecastSolar_MultiPlaneURL(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		fmt.Fprint(w, `{"result":{"watts":{}},"message":{"code":0}}`)
	}))
	defer srv.Close()
	fs := &ForecastSolarProvider{
		Client: srv.Client(), BaseURL: srv.URL,
		Arrays: []Array{
			{TiltDeg: 35, AzimuthDeg: 180, RatedW: 6000}, // south roof
			{TiltDeg: 30, AzimuthDeg: 90, RatedW: 4000},  // east roof
		},
	}
	if _, err := fs.Fetch(context.Background(), 56.7, 16.3); err != nil {
		t.Fatal(err)
	}
	// Expect path to contain all six geometry components in order.
	for _, frag := range []string{"/35.0/0.0/6.00", "/30.0/-90.0/4.00"} {
		if !strings.Contains(seen, frag) {
			t.Errorf("URL missing %q; got %q", frag, seen)
		}
	}
}

func TestForecastSolar_UnequalIntervalsConserveEnergy(t *testing.T) {
	body := `{"result":{"watts":{"2026-04-17T10:10:00+00:00":0,"2026-04-17T10:40:00+00:00":1200,"2026-04-17T11:20:00+00:00":600}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}}
	rows, err := fs.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(rows))
	}
	if got := *rows[0].PVWEstimated; got != 800 {
		t.Errorf("10:00 mean=%g W, want 800 W", got)
	}
	if got := *rows[1].PVWEstimated; got != 200 {
		t.Errorf("11:00 mean=%g W, want 200 W", got)
	}
	var totalWh float64
	for _, row := range rows {
		totalWh += *row.PVWEstimated
	}
	if totalWh != 1000 {
		t.Errorf("hour buckets contain %g Wh, source periods contain 1000 Wh", totalWh)
	}
}

func TestForecastSolar_ExactHourUsesPrecedingPeriod(t *testing.T) {
	body := `{"result":{"watts":{"2026-04-17T12:00:00+00:00":0,"2026-04-17T13:00:00+00:00":750}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}}
	rows, err := fs.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	if len(rows) != 1 || !rows[0].HourStart.Equal(want) || rows[0].PVWEstimated == nil || *rows[0].PVWEstimated != 750 {
		t.Fatalf("preceding 12:00-13:00 period mapped as %+v, want one 12:00 row at 750 W", rows)
	}
}

func TestForecastSolar_InvalidPeriodDoesNotBridgeGap(t *testing.T) {
	body := `{"result":{"watts":{"2026-04-17T10:00:00+00:00":0,"2026-04-17T10:30:00+00:00":-500,"2026-04-17T11:00:00+00:00":1000,"2026-04-17T12:00:00+00:00":1e309,"2026-04-17T13:00:00+00:00":2000}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}}
	rows, err := fs.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%+v, want valid 10:00 and 12:00 periods only", rows)
	}
	if rows[0].HourStart.Hour() != 10 || *rows[0].PVWEstimated != 500 {
		t.Errorf("first valid bucket=%+v, want 10:00 at 500 W", rows[0])
	}
	if rows[1].HourStart.Hour() != 12 || *rows[1].PVWEstimated != 2000 {
		t.Errorf("second valid bucket=%+v, want 12:00 at 2000 W", rows[1])
	}
}

func TestForecastSolar_UTCDoesNotSkipDSTHour(t *testing.T) {
	body := `{"result":{"watts":{"2026-03-29T00:30:00+00:00":0,"2026-03-29T01:30:00+00:00":600}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}}
	rows, err := fs.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].HourStart.Hour() != 0 || rows[1].HourStart.Hour() != 1 {
		t.Fatalf("UTC hours across European DST change: %+v", rows)
	}
	if *rows[0].PVWEstimated != 300 || *rows[1].PVWEstimated != 300 {
		t.Errorf("split UTC energy = %g, %g; want 300, 300 Wh", *rows[0].PVWEstimated, *rows[1].PVWEstimated)
	}
}
