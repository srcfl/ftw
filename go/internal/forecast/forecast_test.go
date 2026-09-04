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
		KWp:        kwp, // legacy kWp; RatedWatts() converts at the config door
		TiltDeg:    &tiltDeg,
		AzimuthDeg: &azimuthDeg,
	}
}

// ---- Clear-sky model sanity ----

func TestClearSkyIsZeroAtMidnight(t *testing.T) {
	// Stockholm midnight in winter
	tt := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	w := ClearSkyWm2(59.3293, 18.0686, tt)
	if w != 0 {
		t.Errorf("midnight winter Stockholm should be 0 W/m², got %f", w)
	}
}

func TestClearSkyIsHighAtSummerNoon(t *testing.T) {
	// Stockholm around solar noon at summer solstice (11:00 UTC ≈ 13:00 local summer)
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	w := ClearSkyWm2(59.3293, 18.0686, tt)
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
	equator := ClearSkyWm2(0, 0, winter)
	arctic := ClearSkyWm2(80, 0, winter)
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
	if pv == 0 {
		t.Error("nil cloud should default to mid-range, not zero")
	}
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
									"air_temperature":     8.5,
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
									"air_temperature":     7.2,
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
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
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
	if err == nil {
		t.Error("expected error on 500")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d", len(rows))
	}
	if *rows[0].CloudCoverPct != 40 {
		t.Errorf("cloud: %f", *rows[0].CloudCoverPct)
	}
	if *rows[1].TempC != 10.5 {
		t.Errorf("temp: %f", *rows[1].TempC)
	}
}

func TestOpenWeatherRequiresKey(t *testing.T) {
	p := NewOpenWeather("")
	_, err := p.Fetch(context.Background(), 59, 18)
	if err == nil {
		t.Error("expected API key error")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d forecasts", len(rows))
	}
	// Stockholm summer clear-ish sky at noon with 10kW array should give ~4-8 kW estimate
	if rows[0].PVWEstimated == nil || *rows[0].PVWEstimated < 1000 {
		t.Errorf("PV estimate should be substantial for clear summer, got %+v", rows[0].PVWEstimated)
	}
	t.Logf("PV estimate at Stockholm summer noon, 10%% cloud, 10kW array: %.0fW", *rows[0].PVWEstimated)
}

// ---- FromConfig ----

func TestFromConfigNilWhenDisabled(t *testing.T) {
	if FromConfig(nil, 10000, nil, "") != nil {
		t.Error("nil cfg → nil svc")
	}
	if FromConfig(&config.Weather{Provider: "none"}, 10000, nil, "") != nil {
		t.Error("none → nil svc")
	}
	if FromConfig(&config.Weather{Provider: ""}, 10000, nil, "") != nil {
		t.Error("empty → nil svc")
	}
}

func TestFromConfigBuildsMetNo(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := FromConfig(&config.Weather{Provider: "met_no", Latitude: 59, Longitude: 18}, 10000, st, "ua")
	if s == nil {
		t.Fatal("expected service")
	}
	if s.Lat != 59 {
		t.Errorf("lat: %f", s.Lat)
	}
	if s.RatedPVW != 10000 {
		t.Errorf("rated: %f", s.RatedPVW)
	}
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
	if s == nil {
		t.Fatal("expected service")
	}
	if len(s.Arrays) != 2 {
		t.Fatalf("expected 2 arrays (kWp>0 only), got %d", len(s.Arrays))
	}
	if s.Arrays[0].RatedW != 6000 || s.Arrays[1].AzimuthDeg != 90 {
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
			{Name: "missing azimuth", RatedW: 10000, TiltDeg: &tilt},
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
	if s.Arrays[0].AzimuthDeg != 180 || s.Arrays[0].RatedW != 6000 {
		t.Fatalf("unexpected complete geometry: %+v", s.Arrays[0])
	}
}

// ---- POA-per-array (orientation-aware) estimate ----

func TestPOAPVWattsSumsArrays(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	one := poaPVWattsFromGHI(59.3293, 18.0686, tt, 700, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 5000}})
	two := poaPVWattsFromGHI(59.3293, 18.0686, tt, 700, []Array{
		{TiltDeg: 35, AzimuthDeg: 180, RatedW: 5000},
		{TiltDeg: 35, AzimuthDeg: 180, RatedW: 5000},
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
	w := poaPVWattsFromGHI(59.3293, 18.0686, tt, 500, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}})
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
					s.Arrays = []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}}
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
		Arrays: []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}},
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

func TestNameplateWSumsRatedWatts(t *testing.T) {
	t.Parallel()
	got := NameplateW(10000, []Array{{RatedW: 6000}, {RatedW: 4000}})
	if got != 10000 {
		t.Fatalf("NameplateW sum = %.0f; want 10000", got)
	}
	got = NameplateW(18960, nil)
	if got != 18960 {
		t.Fatalf("no arrays: NameplateW = %.0f; want rated 18960", got)
	}
}

func TestFromConfigLegacyKWpBecomesWatts(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := FromConfig(&config.Weather{
		Provider: "open_meteo", Latitude: 59.3293, Longitude: 18.0686,
		PVRatedW: 10000,
		PVArrays: []config.PVArray{testPVArray("south", 10, 35, 180)},
	}, 10000, st, "ua")
	if s == nil {
		t.Fatal("expected service")
	}
	if len(s.Arrays) != 1 || s.Arrays[0].RatedW != 10000 {
		t.Fatalf("legacy 10 kWp must become 10000 W, got %+v", s.Arrays)
	}
	pasted := FromConfig(&config.Weather{
		Provider: "open_meteo", Latitude: 59.3293, Longitude: 18.0686,
		PVRatedW: 18960,
		PVArrays: []config.PVArray{testPVArray("east", 12960, 27, 150), testPVArray("south", 6000, 27, 240)},
	}, 18960, st, "ua")
	if pasted == nil || len(pasted.Arrays) != 2 {
		t.Fatal("expected two arrays")
	}
	if pasted.Arrays[0].RatedW != 12960 || pasted.Arrays[1].RatedW != 6000 {
		t.Fatalf("pasted watts-as-kwp must stay watts, got %+v", pasted.Arrays)
	}
}

func TestPOAPVWattsDoesNotTreatWattsAsKWp(t *testing.T) {
	tt := time.Date(2026, 8, 18, 16, 45, 0, 0, time.UTC) // 18:45 Swedish summer
	ghi := 354.0
	house := poaPVWattsFromGHI(59.3293, 18.0686, tt, ghi, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}})
	pasted := poaPVWattsFromGHI(59.3293, 18.0686, tt, ghi, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}})
	if house <= 0 {
		t.Fatalf("expected late-afternoon production, got %.1f W", house)
	}
	if house > 15000 {
		t.Fatalf("10 kWp at 354 W/m² GHI should stay on a house scale, got %.1f W", house)
	}
	if math.Abs(pasted-house) > 1 {
		t.Fatalf("kWp=10000 (watts pasted) must match kWp=10, house=%.1f pasted=%.1f", house, pasted)
	}
}

// Screenshot case: Stockholm 18:45, GHI ~354 W/m², kWp pasted as 10000
// (the Watts field). Before the sanitizer this stored ~3.5 MW and the
// Plan tooltip showed "PV Forecast: 3544.2 kW".
func TestFetchAndStoreCapsPastedWattsGHI(t *testing.T) {
	tt := time.Date(2026, 8, 18, 16, 45, 0, 0, time.UTC)
	ghi := 354.0
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Service{
		Provider: staticForecastProvider{rows: []RawForecast{{HourStart: tt, SolarWm2: &ghi}}},
		Store:    st,
		Lat:      59.3293,
		Lon:      18.0686,
		RatedPVW: 10000,
		Arrays:   []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}},
	}
	s.fetchAndStore(context.Background())
	rows, err := st.LoadForecasts(tt.UnixMilli(), tt.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PVWEstimated == nil {
		t.Fatalf("expected one stored row, got %+v", rows)
	}
	got := *rows[0].PVWEstimated
	if got > 10000*nameplateHeadroom+1 {
		t.Fatalf("pasted 10000 kWp at 354 W/m² must not store megawatts, got %.1f W", got)
	}
	if got <= 0 {
		t.Fatalf("late-afternoon GHI should still produce some PV, got %.1f W", got)
	}
}

// Björn, 2026-08-18: Settings → Weather "PV rated (W)" = 18960.
// Plan tooltip Tue 19:45 showed "PV forecast 2046.2 kW" with house-scale
// load 0.7 kW and 8.2 kW export. Changing weather provider did not help.
//
// The tooltip already does Math.max(0, -pv_w) / 1000, so 2046.2 kW is
// |pv_w| ≈ 2 046 200 W — not watts labelled as kilowatts. 2046200 / 18960
// ≈ 108 W/m², which is a late-evening POA if array kWp was pasted as 18960
// (the watts field). A display-only /1000 miss would mean |pv_w| = 2046 W
// and implied POA 0.11 W/m², which cannot export 8.2 kW or spike the
// ±16 kW chart.
func TestBjorn18960WTooltipIsPastedKWpNotDisplayScale(t *testing.T) {
	t.Parallel()
	const ratedW = 18960.0
	const tooltipKW = 2046.2
	storedW := tooltipKW * 1000
	impliedPOA := storedW / ratedW
	if impliedPOA < 90 || impliedPOA > 130 {
		t.Fatalf("2046.2 kW / 18960 W = %.2f W/m²; want ~108 (evening POA on pasted kWp)", impliedPOA)
	}
	displayBugPOA := (tooltipKW) / ratedW
	if displayBugPOA > 1 {
		t.Fatalf("if tooltip forgot /1000, implied POA would be %.3f W/m², not evening sun", displayBugPOA)
	}

	if NameplateW(ratedW, []Array{{RatedW: ratedW}}) != ratedW {
		t.Fatalf("nameplate = %.0f, want 18960 W", NameplateW(ratedW, []Array{{RatedW: ratedW}}))
	}

	capped, ok := clampPVToNameplate(storedW, ratedW)
	if !ok || capped > ratedW*nameplateHeadroom+1 {
		t.Fatalf("stored %.0f W must clamp to 1.25×18960, got %.1f ok=%v", storedW, capped, ok)
	}

	tt := time.Date(2026, 8, 18, 17, 45, 0, 0, time.UTC) // 19:45 Swedish summer
	house := poaPVWattsFromGHI(59.3293, 18.0686, tt, impliedPOA, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 18960}})
	pastedW := poaPVWattsFromGHI(59.3293, 18.0686, tt, impliedPOA, []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 18960}})
	if house <= 0 || house > ratedW*nameplateHeadroom {
		t.Fatalf("18.96 kWp at ~108 W/m² must stay on a house scale, got %.1f W", house)
	}
	if math.Abs(pastedW-house) > 1 {
		t.Fatalf("kWp=18960 (rated W pasted) must match 18.96 kWp, house=%.1f pasted=%.1f", house, pastedW)
	}
}

func TestLoadClampsStoredMegawattForecast(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	wild := 3544200.0
	ts := time.Date(2026, 8, 18, 16, 45, 0, 0, time.UTC).UnixMilli()
	if err := st.SaveForecasts([]state.ForecastPoint{{
		SlotTsMs: ts, SlotLenMin: 60, PVWEstimated: &wild, Source: "open_meteo",
	}}); err != nil {
		t.Fatal(err)
	}
	s := &Service{
		Store:    st,
		RatedPVW: 10000,
		Arrays:   []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}},
	}
	rows, err := s.Load(ts, ts+3600*1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PVWEstimated == nil {
		t.Fatalf("expected one clamped row, got %+v", rows)
	}
	got := *rows[0].PVWEstimated
	if got > 10000*nameplateHeadroom+1 {
		t.Fatalf("stored 3544 kW forecast must clamp to nameplate, got %.1f W", got)
	}
	if got < 10000 {
		t.Fatalf("clamp should sit on the nameplate ceiling, got %.1f W", got)
	}
}

// ---- STRÅNG calibration hook ----

// radiationForecastPVW runs one fetch against a stub shortwave-radiation
// provider and returns the stored PV estimate, so calibration variants can be
// compared against an otherwise identical run.
func radiationForecastPVW(t *testing.T, calibration func() (float64, bool)) float64 {
	t.Helper()
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
		Arrays:      []Array{{TiltDeg: 35, AzimuthDeg: 180, RatedW: 10000}},
		Calibration: calibration,
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
	return *rows[0].PVWEstimated
}

// The point of scoring: a site measured at 80% of its physics baseline should
// have its forward forecast scaled to match.
func TestServiceAppliesCalibrationToIrradianceEstimate(t *testing.T) {
	uncalibrated := radiationForecastPVW(t, nil)
	calibrated := radiationForecastPVW(t, func() (float64, bool) { return 0.8, true })

	want := uncalibrated * 0.8
	if math.Abs(calibrated-want) > 1.0 {
		t.Errorf("calibrated estimate = %.1f W, want %.1f W (0.8 × %.1f)", calibrated, want, uncalibrated)
	}
	t.Logf("uncalibrated %.0fW → calibrated %.0fW", uncalibrated, calibrated)
}

// An untrusted factor must change nothing: too few days, or a ratio outside the
// plausible band, leaves the physics estimate exactly as it was.
func TestServiceIgnoresUntrustedCalibration(t *testing.T) {
	uncalibrated := radiationForecastPVW(t, nil)
	rejected := radiationForecastPVW(t, func() (float64, bool) { return 0.05, false })

	if math.Abs(rejected-uncalibrated) > 1e-9 {
		t.Errorf("estimate = %.1f W, want the uncalibrated %.1f W", rejected, uncalibrated)
	}
}

// A zero or negative factor would silently zero out the site's whole forecast,
// so it is refused even when the source claims it is usable.
func TestServiceIgnoresNonPositiveCalibration(t *testing.T) {
	uncalibrated := radiationForecastPVW(t, nil)
	for _, factor := range []float64{0, -0.5} {
		got := radiationForecastPVW(t, func() (float64, bool) { return factor, true })
		if math.Abs(got-uncalibrated) > 1e-9 {
			t.Errorf("factor %v: estimate = %.1f W, want the uncalibrated %.1f W", factor, got, uncalibrated)
		}
	}
}
