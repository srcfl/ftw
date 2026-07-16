package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/fleetstats"
)

func relayFleetPayload() fleetstats.Payload {
	return fleetstats.Payload{
		SchemaVersion:  1,
		InstallationID: "00112233445566778899aabbccddeeff",
		Core:           fleetstats.CoreStats{Version: "v1.2.3", Channel: "stable"},
		Optimizer:      &fleetstats.OptimizerStats{Version: "v1.2.4", Transport: "unix", Status: "healthy", ProtocolVersion: 1},
		Drivers: []fleetstats.DriverStats{{
			ID: "ferroamp", Version: "1.0.0", Source: "managed", Status: "healthy",
			Instances: 1, HostAPIMin: 1, HostAPIMax: 1, Kinds: []string{"meter"},
		}},
		SiteMeterHealthy: true,
	}
}

func TestFleetHeartbeatAndAuthenticatedAggregate(t *testing.T) {
	collector, err := fleetstats.NewCollector(filepath.Join(t.TempDir(), "fleet.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	relay := &Relay{Fleet: collector, FleetAdminToken: "admin-secret"}
	handler := relay.Handler()
	raw, _ := json.Marshal(relayFleetPayload())
	req := httptest.NewRequest(http.MethodPost, "/fleet/heartbeat", bytes.NewReader(raw))
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status %d: %s", rec.Code, rec.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/fleet/stats", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/fleet/stats", nil)
	statsReq.Header.Set("Authorization", "Bearer admin-secret")
	statsRec := httptest.NewRecorder()
	handler.ServeHTTP(statsRec, statsReq)
	if statsRec.Code != http.StatusOK {
		t.Fatalf("stats status %d: %s", statsRec.Code, statsRec.Body.String())
	}
	var aggregate fleetstats.Aggregate
	if err := json.NewDecoder(statsRec.Body).Decode(&aggregate); err != nil {
		t.Fatal(err)
	}
	if aggregate.Active24H != 1 || aggregate.Healthy24H != 1 || aggregate.HealthySiteMeters != 1 || aggregate.Drivers["ferroamp"].ActiveInstallations != 1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	if aggregate.CoreChannels["stable"] != 1 || aggregate.OptimizerStatuses["healthy"] != 1 {
		t.Fatalf("component aggregates = %+v", aggregate)
	}
}

func TestFleetHeartbeatRejectsUnknownFields(t *testing.T) {
	collector, err := fleetstats.NewCollector(filepath.Join(t.TempDir(), "fleet.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	handler := (&Relay{Fleet: collector, FleetAdminToken: "admin-secret"}).Handler()
	raw, _ := json.Marshal(relayFleetPayload())
	raw = append(raw[:len(raw)-1], []byte(`,"device_serial":"must-not-pass"}`)...)
	req := httptest.NewRequest(http.MethodPost, "/fleet/heartbeat", bytes.NewReader(raw))
	req.RemoteAddr = "203.0.113.11:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if collector.Aggregate().Active24H != 0 {
		t.Fatal("rejected payload was persisted")
	}
}
