package forecast

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// ---- Clear-sky model sanity ----

func TestClearSkyIsZeroAtMidnight(t *testing.T) {
	// Stockholm midnight in winter
	tt := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	w := ClearSkyW(59.3293, 18.0686, tt)
	if w != 0 {
		t.Errorf("midnight winter Stockholm should be 0 W/m², got %f", w)
	}
}

func TestClearSkyIsHighAtSummerNoon(t *testing.T) {
	// Stockholm around solar noon at summer solstice (11:00 UTC ≈ 13:00 local summer)
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	w := ClearSkyW(59.3293, 18.0686, tt)
	if w < 500 {
		t.Errorf("summer solstice Stockholm should be >500 W/m², got %f", w)
	}
	if w > 1200 {
		t.Errorf("clear-sky should not exceed solar constant, got %f", w)
	}
}

func TestClearSkyLatitudeDependence(t *testing.T) {
	// At winter solstice, equator gets much more sun than high latitudes at noon
	winter := time.Date(2026, 12, 21, 12, 0, 0, 0, time.UTC)
	equator := ClearSkyW(0, 0, winter)
	arctic := ClearSkyW(80, 0, winter)
	if equator <= arctic {
		t.Errorf("equator (%f) should get more winter sun than arctic (%f)", equator, arctic)
	}
	// Arctic in winter: sun below horizon (polar night)
	if arctic != 0 {
		t.Errorf("arctic winter should be 0, got %f", arctic)
	}
}

// ---- PV estimate sanity ----

func TestEstimatePVWZeroAtNight(t *testing.T) {
	tt := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC) // midnight UTC
	cloud := 0.0
	pv := EstimatePVW(59.3293, 18.0686, tt, &cloud, 10000)
	if pv != 0 {
		t.Errorf("night PV should be 0, got %f", pv)
	}
}

func TestEstimatePVWScalesWithRating(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	cloud := 0.0
	pv5k := EstimatePVW(59.3293, 18.0686, tt, &cloud, 5000)
	pv10k := EstimatePVW(59.3293, 18.0686, tt, &cloud, 10000)
	if math.Abs(pv10k/pv5k-2.0) > 0.01 {
		t.Errorf("10 kW array should produce ~2× a 5 kW array, got ratio %f", pv10k/pv5k)
	}
}

func TestEstimatePVWCloudReduction(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	clear := 0.0
	overcast := 100.0
	pvClear := EstimatePVW(59.3293, 18.0686, tt, &clear, 10000)
	pvOver := EstimatePVW(59.3293, 18.0686, tt, &overcast, 10000)
	if pvOver >= pvClear {
		t.Errorf("100%% cloud should be < clear sky, got overcast=%f clear=%f", pvOver, pvClear)
	}
	if pvOver != 0 {
		t.Errorf("our formula: 100%% cloud → 0, got %f", pvOver)
	}
	if pvClear < 3000 {
		t.Errorf("10 kW array on clear summer day at Stockholm should be >3 kW, got %f", pvClear)
	}
}

func TestEstimatePVWNilCloudIsMid(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	pv := EstimatePVW(59.3293, 18.0686, tt, nil, 10000)
	if pv == 0 { t.Error("nil cloud should default to mid-range, not zero") }
}

// ---- met.no HTTP ----

func TestMetNoFetchParses(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("met.no requires User-Agent header, got empty")
		}
		resp := map[string]any{
			"properties": map[string]any{
				"timeseries": []map[string]any{
					{
						"time": "2026-04-14T00:00:00Z",
						"data": map[string]any{
							"instant": map[string]any{
								"details": map[string]any{
									"cloud_area_fraction": 75.0,
									"air_temperature":      8.5,
								},
							},
						},
					},
					{
						"time": "2026-04-14T01:00:00Z",
						"data": map[string]any{
							"instant": map[string]any{
								"details": map[string]any{
									"cloud_area_fraction": 20.0,
									"air_temperature":      7.2,
								},
							},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := NewMetNo("test-ua")
	p.BaseURL = srv.URL
	rows, err := p.Fetch(context.Background(), 59.3, 18.1)
	if err != nil { t.Fatal(err) }
	if len(rows) != 2 { t.Fatalf("got %d rows, want 2", len(rows)) }
	if rows[0].CloudCoverPct == nil || *rows[0].CloudCoverPct != 75 {
		t.Errorf("cloud cover: %+v", rows[0].CloudCoverPct)
	}
	if rows[0].TempC == nil || *rows[0].TempC != 8.5 {
		t.Errorf("temp: %+v", rows[0].TempC)
	}
}

func TestMetNoErrorsOn500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	p := NewMetNo("test")
	p.BaseURL = srv.URL
	_, err := p.Fetch(context.Background(), 59, 18)
	if err == nil { t.Error("expected error on 500") }
}

// ---- OpenWeather HTTP ----

func TestOpenWeatherFetchParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"hourly": []map[string]any{
				{"dt": 1776163200, "temp": 9.1, "clouds": 40.0},
				{"dt": 1776166800, "temp": 10.5, "clouds": 25.0},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	p := NewOpenWeather("test-key")
	p.BaseURL = srv.URL
	rows, err := p.Fetch(context.Background(), 59, 18)
	if err != nil { t.Fatal(err) }
	if len(rows) != 2 { t.Fatalf("got %d", len(rows)) }
	if *rows[0].CloudCoverPct != 40 { t.Errorf("cloud: %f", *rows[0].CloudCoverPct) }
	if *rows[1].TempC != 10.5 { t.Errorf("temp: %f", *rows[1].TempC) }
}

func TestOpenWeatherRequiresKey(t *testing.T) {
	p := NewOpenWeather("")
	_, err := p.Fetch(context.Background(), 59, 18)
	if err == nil { t.Error("expected API key error") }
}

// ---- Service integration ----

func TestServiceFetchesAndStoresWithPVEstimate(t *testing.T) {
	// Mock met.no with a summer-noon slot
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"properties": map[string]any{
				"timeseries": []map[string]any{
					{
						"time": "2026-06-21T11:00:00Z",
						"data": map[string]any{
							"instant": map[string]any{
								"details": map[string]any{
									"cloud_area_fraction": 10.0, // mostly clear
								},
							},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	st, _ := state.Open(filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()

	p := NewMetNo("test")
	p.BaseURL = srv.URL
	s := &Service{Provider: p, Store: st, Lat: 59.3293, Lon: 18.0686, RatedPVW: 10000}
	s.fetchAndStore(context.Background())

	// Load back
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	rows, err := st.LoadForecasts(tt.UnixMilli(), tt.Add(time.Hour).UnixMilli())
	if err != nil { t.Fatal(err) }
	if len(rows) != 1 { t.Fatalf("got %d forecasts", len(rows)) }
	// Stockholm summer clear-ish sky at noon with 10kW array should give ~4-8 kW estimate
	if rows[0].PVWEstimated == nil || *rows[0].PVWEstimated < 1000 {
		t.Errorf("PV estimate should be substantial for clear summer, got %+v", rows[0].PVWEstimated)
	}
	t.Logf("PV estimate at Stockholm summer noon, 10%% cloud, 10kW array: %.0fW", *rows[0].PVWEstimated)
}

// ---- FromConfig ----

func TestFromConfigNilWhenDisabled(t *testing.T) {
	if FromConfig(nil, 10000, nil, "") != nil { t.Error("nil cfg → nil svc") }
	if FromConfig(&config.Weather{Provider: "none"}, 10000, nil, "") != nil { t.Error("none → nil svc") }
	if FromConfig(&config.Weather{Provider: ""}, 10000, nil, "") != nil { t.Error("empty → nil svc") }
}

func TestFromConfigBuildsMetNo(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := FromConfig(&config.Weather{Provider: "met_no", Latitude: 59, Longitude: 18}, 10000, st, "ua")
	if s == nil { t.Fatal("expected service") }
	if s.Lat != 59 { t.Errorf("lat: %f", s.Lat) }
	if s.RatedPVW != 10000 { t.Errorf("rated: %f", s.RatedPVW) }
}
