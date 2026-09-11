package forecast

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Stub server returns a canned Open-Meteo response. Verifies that
// shortwave_radiation, cloud_cover, temperature_2m all land on the
// right RawForecast fields and that timestamps are parsed as UTC.
func TestOpenMeteo_ParsesRadiationAndCloud(t *testing.T) {
	body := `{
		"hourly": {
			"time": ["2026-04-17T12:00","2026-04-17T13:00","2026-04-17T14:00"],
			"shortwave_radiation": [712.5, 685.1, 420.0],
			"cloud_cover": [10.0, 20.0, 45.5],
			"temperature_2m": [18.2, 17.8, 15.9]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "shortwave_radiation") {
			t.Errorf("expected query param shortwave_radiation, got %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 56.7, 16.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if want := time.Date(2026, 4, 17, 11, 0, 0, 0, time.UTC); !rows[0].HourStart.Equal(want) {
		t.Errorf("first interval starts at %s, want %s", rows[0].HourStart, want)
	}
	if rows[0].SolarWm2 == nil || *rows[0].SolarWm2 != 712.5 {
		t.Errorf("SolarWm2 for 11:00-12:00 = %v, want 712.5", rows[0].SolarWm2)
	}
	if rows[0].CloudCoverPct != nil || rows[0].TempC != nil {
		t.Errorf("first interval lacks instant values at its start: cloud=%v temp=%v", rows[0].CloudCoverPct, rows[0].TempC)
	}
	if rows[1].CloudCoverPct == nil || *rows[1].CloudCoverPct != 15 {
		t.Errorf("CloudCoverPct for 12:00-13:00 = %v, want endpoint mean 15", rows[1].CloudCoverPct)
	}
	if rows[2].TempC == nil || *rows[2].TempC != 16.85 {
		t.Errorf("TempC for 13:00-14:00 = %v, want endpoint mean 16.85", rows[2].TempC)
	}
	// PVWEstimated must be nil — this provider only emits radiation;
	// fetchAndStore derives PV from that.
	if rows[0].PVWEstimated != nil {
		t.Errorf("PVWEstimated should be nil for open-meteo, got %v", rows[0].PVWEstimated)
	}
	// Timestamps parsed as UTC.
	if rows[0].HourStart.Location().String() != "UTC" {
		t.Errorf("HourStart.Location = %s, want UTC", rows[0].HourStart.Location())
	}
}

// A row with a missing shortwave_radiation entry (null in the JSON)
// should be surfaced as a nil *float64, not silently zero.
func TestOpenMeteo_HandlesNullFields(t *testing.T) {
	body := `{
		"hourly": {
			"time": ["2026-04-17T12:00", "2026-04-17T13:00"],
			"shortwave_radiation": [null, null],
			"cloud_cover": [40.0, 60.0],
			"temperature_2m": [10.0, 12.0]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].SolarWm2 != nil {
		t.Errorf("null shortwave should be nil, got %v", *rows[0].SolarWm2)
	}
	if rows[0].CloudCoverPct == nil || *rows[0].CloudCoverPct != 50 {
		t.Errorf("CloudCoverPct = %v, want 50", rows[0].CloudCoverPct)
	}
	if rows[0].TempC == nil || *rows[0].TempC != 11 {
		t.Errorf("TempC = %v, want 11", rows[0].TempC)
	}
}

func TestOpenMeteo_DoesNotMoveInstantValuesAcrossSparseGap(t *testing.T) {
	body := `{"hourly":{"time":["2026-04-17T10:00","2026-04-17T12:00"],"shortwave_radiation":[100,300],"cloud_cover":[10,90],"temperature_2m":[5,25]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2 radiation intervals", len(rows))
	}
	if want := time.Date(2026, 4, 17, 11, 0, 0, 0, time.UTC); !rows[1].HourStart.Equal(want) {
		t.Fatalf("second interval starts %s, want %s", rows[1].HourStart, want)
	}
	if rows[1].CloudCoverPct != nil || rows[1].TempC != nil {
		t.Errorf("sparse instants must not be averaged across two hours: cloud=%v temp=%v", rows[1].CloudCoverPct, rows[1].TempC)
	}
}

func TestOpenMeteo_InvalidNumbersStayMissing(t *testing.T) {
	body := `{"hourly":{"time":["2026-04-17T10:00","2026-04-17T11:00","2026-04-17T12:00"],"shortwave_radiation":[0,-1,400],"cloud_cover":[20,120,40],"temperature_2m":[-10,-6,-2]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3", len(rows))
	}
	if rows[1].SolarWm2 != nil || rows[1].CloudCoverPct != nil {
		t.Errorf("negative radiation and out-of-range cloud must stay missing: %+v", rows[1])
	}
	if rows[1].TempC == nil || *rows[1].TempC != -8 {
		t.Errorf("negative air temperature is valid; got %v, want -8", rows[1].TempC)
	}
	if rows[2].CloudCoverPct != nil {
		t.Errorf("one bad cloud endpoint must not manufacture an average: %v", rows[2].CloudCoverPct)
	}
}

func TestOpenMeteo_OverflowInOneFieldDoesNotLoseOtherFields(t *testing.T) {
	body := `{"hourly":{"time":["2026-04-17T10:00","2026-04-17T11:00"],"shortwave_radiation":[0,1e309],"cloud_cover":[20,40],"temperature_2m":[10,12]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(rows))
	}
	if rows[1].SolarWm2 != nil {
		t.Errorf("overflowed radiation must stay missing: %v", rows[1].SolarWm2)
	}
	if rows[1].CloudCoverPct == nil || *rows[1].CloudCoverPct != 30 || rows[1].TempC == nil || *rows[1].TempC != 11 {
		t.Errorf("valid fields beside bad radiation were lost: %+v", rows[1])
	}
}

// Non-200 responses are surfaced as errors — no silent fallback to empty
// rows that would starve downstream consumers without logging.
func TestOpenMeteo_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	if _, err := om.Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error on 403, got nil")
	}
}
