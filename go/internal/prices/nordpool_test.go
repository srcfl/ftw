package prices

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNordPoolParsesDayAhead(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("date") != "2026-09-01" {
			t.Errorf("date = %s", r.URL.Query().Get("date"))
		}
		if r.URL.Query().Get("deliveryArea") != "SE3" {
			t.Errorf("area = %s", r.URL.Query().Get("deliveryArea"))
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent required")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"currency": "SEK",
			"multiAreaEntries": []map[string]any{
				{
					"deliveryStart": "2026-08-31T22:00:00Z",
					"deliveryEnd":   "2026-08-31T22:15:00Z",
					"entryPerArea":  map[string]float64{"SE3": 1243.02},
				},
				{
					"deliveryStart": "2026-08-31T22:15:00Z",
					"deliveryEnd":   "2026-08-31T22:30:00Z",
					"entryPerArea":  map[string]float64{"SE3": 1083.3},
				},
			},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &NordPoolProvider{Client: srv.Client(), BaseURL: srv.URL, Currency: "SEK"}
	day := time.Date(2026, 9, 1, 8, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	rows, err := p.Fetch(context.Background(), "se3", day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].SlotLenMin != 15 {
		t.Errorf("slot = %d", rows[0].SlotLenMin)
	}
	if rows[0].SEKPerKWh < 1.24 || rows[0].SEKPerKWh > 1.25 {
		t.Errorf("SEK/kWh = %g", rows[0].SEKPerKWh)
	}
}

func TestNordPoolUnpublishedDay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p := &NordPoolProvider{Client: srv.Client(), BaseURL: srv.URL, Currency: "SEK"}
	rows, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err != nil {
		t.Fatalf("404 should not error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d", len(rows))
	}
}

func TestSourcefulFallsBackToNordPool(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"currency": "SEK",
			"multiAreaEntries": []map[string]any{{
				"deliveryStart": "2026-08-31T22:00:00Z",
				"deliveryEnd":   "2026-08-31T22:15:00Z",
				"entryPerArea":  map[string]float64{"SE3": 1000},
			}},
		})
	}))
	defer secondary.Close()
	p := withFallback(
		&SourcefulProvider{Client: primary.Client(), BaseURL: primary.URL},
		&NordPoolProvider{Client: secondary.Client(), BaseURL: secondary.URL, Currency: "SEK"},
	)
	if p.Name() != "sourceful" {
		t.Errorf("name = %s", p.Name())
	}
	rows, err := p.Fetch(context.Background(), "SE3", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d, want Nord Pool row", len(rows))
	}
}

func TestNextDayAheadCatch(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 8, 31, 12, 0, 0, 0, loc)
	got := nextDayAheadCatch(before)
	want := time.Date(2026, 8, 31, 13, 5, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("before publication: got %s want %s", got, want)
	}
	after := time.Date(2026, 8, 31, 13, 6, 0, 0, loc)
	got = nextDayAheadCatch(after)
	want = time.Date(2026, 9, 1, 13, 5, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("after publication: got %s want %s", got, want)
	}
}

func TestNordPoolRejectsCurrencyMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"currency":         "EUR",
			"multiAreaEntries": []any{},
		})
	}))
	defer srv.Close()
	p := &NordPoolProvider{Client: srv.Client(), BaseURL: srv.URL, Currency: "SEK"}
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil || !strings.Contains(err.Error(), "asked for SEK") {
		t.Fatalf("err = %v", err)
	}
}
