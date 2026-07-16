package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/fleetstats"
)

type fleetMemoryStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *fleetMemoryStore) LoadConfig(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.m[key]
	return value, ok
}

func (s *fleetMemoryStore) SaveConfig(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]string)
	}
	s.m[key] = value
	return nil
}

func apiFleetPayload() fleetstats.Payload {
	return fleetstats.Payload{
		Core: fleetstats.CoreStats{Version: "v1.2.3", Channel: "stable"},
		Drivers: []fleetstats.DriverStats{{
			ID: "ferroamp", Version: "1.0.0", Source: "managed", Status: "healthy",
			Instances: 1, HostAPIMin: 1, HostAPIMax: 1, Kinds: []string{"meter"},
		}},
		SiteMeterHealthy: true,
	}
}

func TestFleetStatisticsPreviewAndManualSubmit(t *testing.T) {
	var received fleetstats.Payload
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer collector.Close()
	reporter, err := fleetstats.NewReporter(fleetstats.Config{
		Enabled: true, Endpoint: collector.URL, AllowInsecure: true, HTTPClient: collector.Client(),
	}, &fleetMemoryStore{}, func(context.Context) (fleetstats.Payload, error) { return apiFleetPayload(), nil })
	if err != nil {
		t.Fatal(err)
	}
	server := New(&Deps{FleetStats: reporter})

	previewRec := httptest.NewRecorder()
	server.mux.ServeHTTP(previewRec, httptest.NewRequest(http.MethodGet, "/api/fleet_statistics/preview", nil))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status %d: %s", previewRec.Code, previewRec.Body.String())
	}
	var preview struct {
		Enabled bool               `json:"enabled"`
		Payload fleetstats.Payload `json:"payload"`
	}
	if err := json.NewDecoder(previewRec.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Enabled || preview.Payload.InstallationID == "" {
		t.Fatalf("preview = %+v", preview)
	}

	submitRec := httptest.NewRecorder()
	server.mux.ServeHTTP(submitRec, httptest.NewRequest(http.MethodPost, "/api/fleet_statistics/submit", nil))
	if submitRec.Code != http.StatusOK {
		t.Fatalf("submit status %d: %s", submitRec.Code, submitRec.Body.String())
	}
	if received.InstallationID != preview.Payload.InstallationID || received.Core.Version != "v1.2.3" {
		t.Fatalf("received = %+v, preview = %+v", received, preview.Payload)
	}
}

func TestFleetStatisticsSubmitIsBlockedWhenDisabled(t *testing.T) {
	reporter, err := fleetstats.NewReporter(fleetstats.Config{}, &fleetMemoryStore{}, func(context.Context) (fleetstats.Payload, error) {
		return apiFleetPayload(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(&Deps{FleetStats: reporter})
	rec := httptest.NewRecorder()
	server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/fleet_statistics/submit", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
