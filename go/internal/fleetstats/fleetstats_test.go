package fleetstats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type memoryStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memoryStore) LoadConfig(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.m[key]
	return value, ok
}

func (s *memoryStore) SaveConfig(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]string)
	}
	s.m[key] = value
	return nil
}

func healthyPayload() Payload {
	return Payload{
		Core:      CoreStats{Version: "v1.2.3", Channel: "stable"},
		Optimizer: &OptimizerStats{Version: "v1.2.3", Transport: "unix", Status: "healthy", ProtocolVersion: 1},
		Drivers: []DriverStats{{
			ID: "ferroamp", Version: "1.0.0", Source: "managed", Status: "healthy",
			Instances: 1, HostAPIMin: 1, HostAPIMax: 1, Kinds: []string{"battery", "meter", "pv"},
		}},
		SiteMeterHealthy: true,
	}
}

func TestReporterPreviewMatchesSubmittedPayload(t *testing.T) {
	var received Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &memoryStore{}
	reporter, err := NewReporter(Config{
		Enabled: true, Endpoint: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	}, store, func(context.Context) (Payload, error) { return healthyPayload(), nil })
	if err != nil {
		t.Fatal(err)
	}
	preview, err := reporter.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := reporter.Submit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview, submitted) || !reflect.DeepEqual(submitted, received) {
		t.Fatalf("preview/submitted/received differ:\n%+v\n%+v\n%+v", preview, submitted, received)
	}
	if preview.InstallationID == "" || preview.InstallationID != store.m[installationIDKey] {
		t.Fatal("anonymous installation id was not persisted")
	}
}

func TestReporterDisabledPerformsNoSubmission(t *testing.T) {
	calls := 0
	reporter, err := NewReporter(Config{}, &memoryStore{}, func(context.Context) (Payload, error) {
		calls++
		return healthyPayload(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Run(context.Background())
	if calls != 0 {
		t.Fatalf("disabled reporter built or sent %d payloads", calls)
	}
}

func TestPayloadRejectsIdentityAndSecretShapedValues(t *testing.T) {
	for _, value := range []string{
		"user@example.com", "serial:ABC123", "192.168.1.10", "host/name", "token=secret", "password",
	} {
		payload := healthyPayload()
		payload.SchemaVersion = SchemaVersion
		payload.InstallationID = "00112233445566778899aabbccddeeff"
		payload.Drivers[0].ID = value
		if err := payload.Validate(); err == nil {
			t.Errorf("privacy-unsafe driver id %q accepted", value)
		}
	}
}

func TestBuildSnapshotDropsLocalNamesAndConfig(t *testing.T) {
	now := time.Now()
	payload := BuildSnapshot(SnapshotInput{
		CoreVersion: "dev", Channel: "edge",
		Drivers: []config.Driver{{
			Name: "Fredriks garage 192.168.1.20", Lua: "/app/drivers/ferroamp.lua", IsSiteMeter: true,
			Config: map[string]any{"password": "secret", "serial": "ABC123"},
		}},
		Catalog: []drivers.CatalogEntry{{
			ID: "ferroamp", Filename: "ferroamp.lua", Version: "1.0.0", Source: "bundled",
			HostAPIMin: 1, HostAPIMax: 1, Capabilities: []string{"meter", "battery"},
		}},
		Health: map[string]telemetry.DriverHealth{
			"Fredriks garage 192.168.1.20": {Name: "Fredriks garage 192.168.1.20", Status: telemetry.StatusOk, LastSuccess: &now},
		},
	})
	raw, _ := json.Marshal(payload)
	for _, forbidden := range []string{"Fredrik", "192.168", "secret", "ABC123", "password", "serial"} {
		if stringContains(string(raw), forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, raw)
		}
	}
	if !payload.SiteMeterHealthy || len(payload.Drivers) != 1 || payload.Drivers[0].Status != "healthy" {
		t.Fatalf("snapshot = %+v", payload)
	}
}

func TestBuildSnapshotCountsButNeverNamesLocalCustomDriver(t *testing.T) {
	now := time.Now()
	payload := BuildSnapshot(SnapshotInput{
		CoreVersion: "dev",
		Drivers:     []config.Driver{{Name: "private", Lua: "/data/drivers/fredriks-lab.lua", IsSiteMeter: true}},
		Catalog: []drivers.CatalogEntry{{
			ID: "fredriks-private-lab", Filename: "fredriks-lab.lua", Version: "1.0.0",
			Source: "local", HostAPIMin: 1, HostAPIMax: 1,
		}},
		Health: map[string]telemetry.DriverHealth{
			"private": {Name: "private", Status: telemetry.StatusOk, LastSuccess: &now},
		},
	})
	raw, _ := json.Marshal(payload)
	if len(payload.Drivers) != 0 || payload.UnidentifiedDriverCount != 1 {
		t.Fatalf("snapshot = %+v", payload)
	}
	if stringContains(string(raw), "fredrik") || stringContains(string(raw), "private") {
		t.Fatalf("local custom driver identity leaked: %s", raw)
	}
	if !payload.SiteMeterHealthy {
		t.Fatal("healthy local site meter should still count as a healthy anonymous installation")
	}
}

func TestCollectorAggregatesActiveInstallationsAndDriverHealth(t *testing.T) {
	collector, err := NewCollector(filepath.Join(t.TempDir(), "fleet.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	collector.now = func() time.Time { return now }
	for i, healthy := range []bool{true, false} {
		payload := healthyPayload()
		payload.SchemaVersion = SchemaVersion
		if i == 0 {
			payload.InstallationID = "00112233445566778899aabbccddeeff"
			payload.Drivers[0].Instances = 2
		} else {
			payload.InstallationID = "ffeeddccbbaa99887766554433221100"
			payload.Drivers[0].Status = "offline"
		}
		payload.SiteMeterHealthy = healthy
		if err := collector.Record(payload); err != nil {
			t.Fatal(err)
		}
	}
	aggregate := collector.Aggregate()
	if aggregate.Active24H != 2 || aggregate.Healthy24H != 1 || aggregate.HealthySiteMeters != 1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	driver := aggregate.Drivers["ferroamp"]
	if driver.ActiveInstallations != 2 || driver.ConfiguredInstances != 3 || driver.HealthyInstances != 2 || driver.Versions["1.0.0"] != 2 {
		t.Fatalf("driver aggregate = %+v", driver)
	}

	reloaded, err := NewCollector(collector.path, 100)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = collector.now
	if reloaded.Aggregate().Active24H != 2 {
		t.Fatal("persisted collector state did not reload")
	}
}

func stringContains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
