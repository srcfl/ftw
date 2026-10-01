package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/prices"
)

func TestBootstrapPriceZones(t *testing.T) {
	rr := httptest.NewRecorder()
	secureBootstrapMutations(http.HandlerFunc(bootstrapPriceZones)).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/prices/zones", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Zones []struct{ Code, Country, Currency, Name string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := prices.Zones()
	if len(result.Zones) != len(want) {
		t.Fatalf("got %d zones, want %d", len(result.Zones), len(want))
	}
	for i, zone := range result.Zones {
		if zone.Code != want[i].Code || zone.Country != want[i].Country || zone.Currency != want[i].Currency || zone.Name != want[i].Name() {
			t.Errorf("zone %d = %+v, want %+v", i, zone, want[i])
		}
	}
}
