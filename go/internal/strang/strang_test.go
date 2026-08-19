package strang

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/sunpos"
)

func TestFetchWindowMergesGHIAndDHI(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var arr []map[string]any
		switch {
		case strings.Contains(r.URL.Path, "/parameter/117/"):
			arr = []map[string]any{
				{"date_time": "2024-06-01T10:00:00Z", "value": 500.0},
				{"date_time": "2024-06-01T11:00:00Z", "value": 650.0},
			}
		case strings.Contains(r.URL.Path, "/parameter/122/"):
			arr = []map[string]any{
				{"date_time": "2024-06-01T10:00:00Z", "value": 120.0},
				{"date_time": "2024-06-01T11:00:00Z", "value": 150.0},
			}
		}
		_ = json.NewEncoder(w).Encode(arr)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := NewClient("test")
	c.BaseURL = srv.URL
	hours, err := c.FetchWindow(context.Background(), 59.33, 18.07,
		time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 2 {
		t.Fatalf("got %d hours, want 2", len(hours))
	}
	if hours[0].GHIWm2 != 500 || hours[1].GHIWm2 != 650 {
		t.Errorf("GHI mismatch: %+v", hours)
	}
	if hours[0].DHIWm2 == nil || *hours[0].DHIWm2 != 120 {
		t.Errorf("DHI[0] mismatch: %+v", hours[0])
	}
	if !hours[0].HourStart.Before(hours[1].HourStart) {
		t.Error("hours should be ascending")
	}
}

// Diffuse (122) unavailable — common for pre-2017 windows — must not fail the
// window; GHI still returns with nil DHI.
func TestFetchWindowDiffuseErrorTolerated(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/parameter/122/") {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"date_time": "2016-06-01T10:00:00Z", "value": 480.0},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := NewClient("test")
	c.BaseURL = srv.URL
	hours, err := c.FetchWindow(context.Background(), 59, 18,
		time.Date(2016, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2016, 6, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("diffuse error should not fail window: %v", err)
	}
	if len(hours) != 1 || hours[0].DHIWm2 != nil {
		t.Errorf("expected 1 hour with nil DHI, got %+v", hours)
	}
}

// Global (117) error must fail the window — GHI is required.
func TestFetchWindowGlobalErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := NewClient("t")
	c.BaseURL = srv.URL
	_, err := c.FetchWindow(context.Background(), 59, 18,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Error("global irradiance error should fail the window")
	}
}

// Null values in the series are skipped, not decoded as 0.
func TestFetchWindowSkipsNullValues(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/parameter/122/") {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"date_time": "2024-06-01T10:00:00Z", "value": 500.0},
			{"date_time": "2024-06-01T11:00:00Z", "value": nil},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := NewClient("t")
	c.BaseURL = srv.URL
	hours, err := c.FetchWindow(context.Background(), 59, 18,
		time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 1 || hours[0].GHIWm2 != 500 {
		t.Errorf("null value should be skipped, got %+v", hours)
	}
}

// --- cloud cover derived from sunshine duration (parameter 119) ---

func minutesPtr(v float64) *float64 { return &v }

const (
	sthlmLat = 59.33
	sthlmLon = 18.07
)

// Midsummer noon and midnight at Stockholm: unambiguously day and night.
var (
	noonUTC     = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	midnightUTC = time.Date(2026, 12, 21, 23, 0, 0, 0, time.UTC)
)

func TestCloudCoverFromSunshineDuration(t *testing.T) {
	cases := []struct {
		name    string
		minutes *float64
		want    float64
		wantOK  bool
	}{
		{"full hour of sun is clear sky", minutesPtr(60), 0, true},
		{"no sun at all is overcast", minutesPtr(0), 1, true},
		{"half an hour is half cover", minutesPtr(30), 0.5, true},
		{"quarter hour is three quarters cover", minutesPtr(15), 0.75, true},
		{"missing parameter is unknown, not clear", nil, 0, false},
		{"negative is rejected as unknown", minutesPtr(-1), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := IrradianceHour{HourStart: noonUTC, SunshineMin: c.minutes}
			got, ok := h.CloudCover(sthlmLat, sthlmLon)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("cover = %v, want %v", got, c.want)
			}
		})
	}
}

// A value above 60 would push cover negative and read as "brighter than clear",
// which is meaningless. Clamp instead.
func TestCloudCoverClampsAboveFullHour(t *testing.T) {
	h := IrradianceHour{HourStart: noonUTC, SunshineMin: minutesPtr(75)}
	got, ok := h.CloudCover(sthlmLat, sthlmLon)
	if !ok {
		t.Fatal("want derivable")
	}
	if got != 0 {
		t.Errorf("cover = %v, want 0 (clamped)", got)
	}
}

// The distinction that matters at a call site: unknown must never be mistaken
// for clear, because they lead to opposite decisions.
func TestCloudCoverUnknownIsDistinguishableFromClear(t *testing.T) {
	unknown, okU := IrradianceHour{HourStart: noonUTC}.CloudCover(sthlmLat, sthlmLon)
	clear, okC := IrradianceHour{HourStart: noonUTC, SunshineMin: minutesPtr(60)}.CloudCover(sthlmLat, sthlmLon)
	if okU {
		t.Error("absent sunshine must report not-ok")
	}
	if !okC {
		t.Error("full sun must report ok")
	}
	if unknown != clear {
		t.Log("values differ, but callers must branch on the boolean, not the value")
	}
}

// Outside the Nordic domain STRÅNG can never return data, so the client must
// refuse locally rather than spend three HTTP requests learning that.
func TestFetchWindowRefusesOutsideDomain(t *testing.T) {
	c := NewClient("test")
	c.BaseURL = "http://127.0.0.1:1" // must never be dialled
	_, err := c.FetchWindow(context.Background(), -33.87, 151.21,
		time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("want an error for Sydney")
	}
	if !errors.Is(err, ErrOutsideDomain) {
		t.Errorf("err = %v, want ErrOutsideDomain", err)
	}
}

func TestFetchWindowAcceptsInsideDomain(t *testing.T) {
	// Stockholm is in-domain, so this must get past the guard and fail on the
	// unreachable transport instead.
	c := NewClient("test")
	c.BaseURL = "http://127.0.0.1:1"
	_, err := c.FetchWindow(context.Background(), 59.33, 18.07,
		time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	if errors.Is(err, ErrOutsideDomain) {
		t.Fatal("Stockholm must not be rejected as outside the domain")
	}
}

// Sunshine duration is zero at night because there is no sun, not because it is
// overcast. Reporting 100% cover would be confidently wrong every single night,
// which is exactly what the live API returned before this guard existed.
func TestCloudCoverIsUnknownAtNight(t *testing.T) {
	h := IrradianceHour{HourStart: midnightUTC, SunshineMin: minutesPtr(0)}
	if _, ok := h.CloudCover(sthlmLat, sthlmLon); ok {
		t.Error("midwinter midnight must report unknown, not 100% cloud")
	}
}

// The hour the sun climbs through the threshold must be answerable, and it is
// only answerable because all three sample points are checked. Midsummer at
// Stockholm, 02:00Z: elevation runs 1.59 deg at :00 and 4.34 deg at :30 — both
// below the 5 deg cutoff — reaching 7.25 deg by :59. Sampling the start or the
// midpoint alone would discard a genuinely observed half-hour of sunshine.
func TestCloudCoverCountsHourWhereSunCrossesThreshold(t *testing.T) {
	start := time.Date(2026, 6, 21, 2, 0, 0, 0, time.UTC)
	if sunpos.At(start, sthlmLat, sthlmLon).ZenithDeg < 90-minSunElevationDeg {
		t.Fatal("premise broken: the sun should start this hour below the cutoff")
	}
	h := IrradianceHour{HourStart: start, SunshineMin: minutesPtr(30)}
	got, ok := h.CloudCover(sthlmLat, sthlmLon)
	if !ok {
		t.Fatal("the hour the sun crosses the cutoff should be derivable")
	}
	if got != 0.5 {
		t.Errorf("cover = %v, want 0.5", got)
	}
}

// Polar night: the sun never rises, so no hour of the day is derivable.
func TestCloudCoverUnknownThroughPolarNight(t *testing.T) {
	const tromsoLat, tromsoLon = 69.65, 18.96
	for hour := 0; hour < 24; hour++ {
		h := IrradianceHour{
			HourStart:   time.Date(2026, 12, 21, hour, 0, 0, 0, time.UTC),
			SunshineMin: minutesPtr(0),
		}
		if _, ok := h.CloudCover(tromsoLat, tromsoLon); ok {
			t.Errorf("hour %02d: polar night must report unknown", hour)
		}
	}
}

// Near sunrise/sunset the beam crosses too much atmosphere to clear the WMO
// threshold even under a clear sky, so a zero reading there describes geometry
// rather than cloud. Verified against the live API: 2026-06-21 20:00Z at
// Stockholm has GHI 2.5 W/m2 and 0 minutes of sunshine — the sun is minutes
// from setting, and calling that "100% overcast" would be wrong.
func TestCloudCoverDeclinesNearTheHorizon(t *testing.T) {
	h := IrradianceHour{
		HourStart:   time.Date(2026, 6, 21, 20, 0, 0, 0, time.UTC),
		SunshineMin: minutesPtr(0),
	}
	if _, ok := h.CloudCover(sthlmLat, sthlmLon); ok {
		t.Error("a sun about to set must report unknown, not fully overcast")
	}
}

// ...but a genuinely low-yet-usable sun must still be answerable, otherwise the
// guard would silently discard most of a Nordic winter.
func TestCloudCoverStillAnswersWhenSunIsUsablyUp(t *testing.T) {
	// 2026-06-21 04:00Z at Stockholm: live GHI 163.2 W/m2, 60 min sunshine.
	h := IrradianceHour{
		HourStart:   time.Date(2026, 6, 21, 4, 0, 0, 0, time.UTC),
		SunshineMin: minutesPtr(60),
	}
	got, ok := h.CloudCover(sthlmLat, sthlmLon)
	if !ok {
		t.Fatal("a usable morning sun must be derivable")
	}
	if got != 0 {
		t.Errorf("cover = %v, want 0 (full sunshine)", got)
	}
}
