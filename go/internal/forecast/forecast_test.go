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

type staticForecastProvider struct {
	rows []RawForecast
}

func (p staticForecastProvider) Name() string { return "static" }

func (p staticForecastProvider) Fetch(context.Context, float64, float64) ([]RawForecast, error) {
	return p.rows, nil
}

func testPVArray(name string, kwp, tiltDeg, azimuthDeg float64) config.PVArray {
	return config.PVArray{
		Name:       name,
		KWp:        kwp,
		TiltDeg:    &tiltDeg,
		AzimuthDeg: &azimuthDeg,
	}
}

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

func TestFromConfigPopulatesArrays(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	cfg := &config.Weather{
		Provider: "open_meteo", Latitude: 59, Longitude: 18,
		PVArrays: []config.PVArray{
			testPVArray("south", 6, 35, 180),
			testPVArray("east", 4, 30, 90),
			testPVArray("empty", 0, 10, 200), // skipped (kWp 0)
		},
	}
	s := FromConfig(cfg, 10000, st, "ua")
	if s == nil { t.Fatal("expected service") }
	if len(s.Arrays) != 2 {
		t.Fatalf("expected 2 arrays (kWp>0 only), got %d", len(s.Arrays))
	}
	if s.Arrays[0].KWp != 6 || s.Arrays[1].AzimuthDeg != 90 {
		t.Errorf("array geometry mismatch: %+v", s.Arrays)
	}
}

func TestFromConfigSkipsPartialArrayGeometry(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tilt := 35.0
	cfg := &config.Weather{
		Provider: "open_meteo", Latitude: 59.3293, Longitude: 18.0686,
		PVArrays: []config.PVArray{
			{Name: "missing azimuth", KWp: 10, TiltDeg: &tilt},
			testPVArray("Stockholm south", 6, 35, 180),
		},
	}
	s := FromConfig(cfg, 16000, st, "ua")
	if s == nil {
		t.Fatal("expected service")
	}
	if len(s.Arrays) != 1 {
		t.Fatalf("expected only complete Stockholm geometry, got %d arrays: %+v", len(s.Arrays), s.Arrays)
	}
	if s.Arrays[0].AzimuthDeg != 180 || s.Arrays[0].KWp != 6 {
		t.Fatalf("unexpected complete geometry: %+v", s.Arrays[0])
	}
}

// ---- POA-per-array (orientation-aware) estimate ----

func TestPOAPVWattsSumsArrays(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	one := poaPVWattsFromGHI(59.3293, 18.0686, tt, 700, []Array{{TiltDeg: 35, AzimuthDeg: 180, KWp: 5}})
	two := poaPVWattsFromGHI(59.3293, 18.0686, tt, 700, []Array{
		{TiltDeg: 35, AzimuthDeg: 180, KWp: 5},
		{TiltDeg: 35, AzimuthDeg: 180, KWp: 5},
	})
	if one <= 0 {
		t.Fatalf("expected positive POA watts, got %.1f", one)
	}
	if math.Abs(two-2*one) > 1e-6 {
		t.Errorf("two identical arrays should double output: one=%.2f two=%.2f", one, two)
	}
}

func TestPOAPVWattsZeroAtNight(t *testing.T) {
	tt := time.Date(2026, 12, 21, 23, 0, 0, 0, time.UTC)
	w := poaPVWattsFromGHI(59.3293, 18.0686, tt, 500, []Array{{TiltDeg: 35, AzimuthDeg: 180, KWp: 10}})
	if w != 0 {
		t.Errorf("night POA watts should be 0, got %.2f", w)
	}
}

func TestServiceGHIPhysicalBounds(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		ghi       float64
		valid     bool
		wantSolar float64
	}{
		{name: "negative", ghi: -100, valid: true, wantSolar: 0},
		{name: "zero", ghi: 0, valid: true, wantSolar: 0},
		{name: "nan", ghi: math.NaN(), valid: false},
		{name: "positive infinity", ghi: math.Inf(1), valid: false},
	}
	for _, withArrays := range []bool{false, true} {
		path := "without arrays"
		if withArrays {
			path = "with arrays"
		}
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				ghi := tc.ghi
				s := &Service{
					Provider: staticForecastProvider{rows: []RawForecast{{HourStart: tt, SolarWm2: &ghi}}},
					Store:    st,
					Lat:      59.3293,
					Lon:      18.0686,
					RatedPVW: 10000,
					Arrays:   nil,
				}
				if withArrays {
					s.Arrays = []Array{{TiltDeg: 35, AzimuthDeg: 180, KWp: 10}}
				}
				s.fetchAndStore(context.Background())

				rows, err := st.LoadForecasts(tt.UnixMilli(), tt.Add(time.Hour).UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if !tc.valid {
					if len(rows) != 0 {
						t.Fatalf("non-finite irradiance should omit the row, got %+v", rows)
					}
					return
				}
				if len(rows) != 1 || rows[0].PVWEstimated == nil || rows[0].SolarWm2 == nil {
					t.Fatalf("expected one finite forecast row, got %+v", rows)
				}
				if got := *rows[0].PVWEstimated; got != 0 {
					t.Errorf("PV estimate from %s irradiance = %.2f, want 0", tc.name, got)
				}
				if got := *rows[0].SolarWm2; got != tc.wantSolar {
					t.Errorf("stored irradiance = %.2f, want %.2f", got, tc.wantSolar)
				}
			})
		}
	}
}

// End-to-end: with per-plane geometry, a GHI-bearing provider's stored PV
// estimate comes from the POA path and differs from the flat rated×GHI/1000.
func TestServicePOAPathDiffersFromFlat(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"hourly": map[string]any{
				"time":                []string{"2026-06-21T11:00"},
				"shortwave_radiation": []float64{700},
				"cloud_cover":         []float64{5},
				"temperature_2m":      []float64{20},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	st, _ := state.Open(filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()

	p := NewOpenMeteo()
	p.BaseURL = srv.URL
	s := &Service{
		Provider: p, Store: st, Lat: 59.3293, Lon: 18.0686, RatedPVW: 10000,
		Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, KWp: 10}},
	}
	s.fetchAndStore(context.Background())

	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	rows, err := st.LoadForecasts(tt.UnixMilli(), tt.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PVWEstimated == nil {
		t.Fatalf("expected 1 forecast with PV estimate, got %+v", rows)
	}
	got := *rows[0].PVWEstimated
	flat := 10000 * 700.0 / 1000.0 // orientation-blind estimate = 7000 W
	want := poaPVWattsFromGHI(59.3293, 18.0686, tt, 700, s.Arrays)
	if math.Abs(got-want) > 1.0 {
		t.Errorf("service should use POA path: got %.1f want %.1f", got, want)
	}
	if math.Abs(got-flat) < 1.0 {
		t.Errorf("POA estimate should differ from flat %.0f, got %.1f", flat, got)
	}
	t.Logf("POA-per-array estimate %.0fW vs flat %.0fW", got, flat)
}
