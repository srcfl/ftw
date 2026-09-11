package evcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestTeslaWCListChargers(t *testing.T) {
	var versionHits, vitalsHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/1/version":
			versionHits++
			_ = json.NewEncoder(w).Encode(map[string]string{
				"serial_number": "TWCabc123",
				"part_number":   "1529455-02-J",
				"firmware_version": "22.36.1",
			})
		case "/api/1/vitals":
			vitalsHits++
			_ = json.NewEncoder(w).Encode(map[string]any{"evse_state": 1})
		default:
			http.Error(w, "unknown", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := NewTeslaWC().WithHTTPClient(srv.Client())
	got, err := p.ListChargers(&config.EVCharger{
		Provider: "tesla-wc",
		HTTP:     &config.EVChargerHTTP{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("chargers: got %d, want 1", len(got))
	}
	if got[0].ID != "TWCabc123" {
		t.Errorf("id: got %q, want TWCabc123", got[0].ID)
	}
	if !strings.Contains(got[0].Name, "1529455-02-J") {
		t.Errorf("name: got %q, want part number", got[0].Name)
	}
	if versionHits != 1 {
		t.Errorf("version hits=%d, want 1", versionHits)
	}
	if vitalsHits != 0 {
		t.Errorf("vitals should not be called when version has a serial, hits=%d", vitalsHits)
	}
}

func TestTeslaWCListChargersVitalsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/1/version" {
			http.Error(w, "no version", http.StatusNotFound)
			return
		}
		if r.URL.Path == "/api/1/vitals" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"vehicle_connected": false,
				"evse_state":        1,
			})
			return
		}
		http.Error(w, "unknown", http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewTeslaWC().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	got, err := p.ListChargers(&config.EVCharger{Provider: "tesla-wc"})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tesla-wc" {
		t.Fatalf("got %+v, want fallback id tesla-wc", got)
	}
}

func TestTeslaWCListChargersRequiresBaseURL(t *testing.T) {
	_, err := NewTeslaWC().ListChargers(&config.EVCharger{Provider: "tesla-wc"})
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("expected base_url error, got %v", err)
	}
}

func TestTeslaWCNormalizeBareHost(t *testing.T) {
	if got := teslaWCNormalizeBase("192.168.1.50"); got != "http://192.168.1.50" {
		t.Errorf("bare host: got %q", got)
	}
	if got := teslaWCNormalizeBase("http://192.168.1.50/"); got != "http://192.168.1.50" {
		t.Errorf("slash: got %q", got)
	}
}

func TestTeslaWCTimeoutBounded(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	block := make(chan struct{})
	defer close(block)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	p := NewTeslaWC().WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}).WithBaseURL(srv.URL)
	done := make(chan error, 1)
	go func() {
		_, err := p.ListChargers(&config.EVCharger{Provider: "tesla-wc"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListChargers did not return within 2s")
	}
}

func TestTeslaWCDescribe(t *testing.T) {
	d := NewTeslaWC().Describe()
	if d.Name != "tesla-wc" || d.Label != "Tesla Wall Connector" {
		t.Errorf("name/label: %+v", d)
	}
	if d.Transport != TransportHTTP {
		t.Errorf("transport: %s", d.Transport)
	}
	if d.NeedsAuth {
		t.Error("local wall connector must not require auth")
	}
	if d.LuaDriver != "drivers/tesla_wall_connector.lua" {
		t.Errorf("lua: %q", d.LuaDriver)
	}
}

func TestTeslaWCListChargersVitalsNaNFallback(t *testing.T) {
	// Version missing + vitals with the documented bare-nan quirk must
	// still list the box. The Lua driver already repairs this; the
	// wizard probe used to fail on the same body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/1/version" {
			http.Error(w, "no version", http.StatusNotFound)
			return
		}
		if r.URL.Path == "/api/1/vitals" {
			_, _ = w.Write([]byte(`{"vehicle_connected":false,"evse_state":1,"grid_v":nan}`))
			return
		}
		http.Error(w, "unknown", http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewTeslaWC().WithHTTPClient(srv.Client()).WithBaseURL(srv.URL)
	got, err := p.ListChargers(&config.EVCharger{Provider: "tesla-wc"})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tesla-wc" {
		t.Fatalf("got %+v, want fallback id tesla-wc", got)
	}
}

func TestTeslaWCListChargersCamelCaseSerial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/1/version" {
			http.Error(w, "unknown", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"serialNumber": "TWCcamel",
			"partNumber":   "1529455-02-J",
		})
	}))
	defer srv.Close()

	got, err := NewTeslaWC().WithHTTPClient(srv.Client()).ListChargers(&config.EVCharger{
		Provider: "tesla-wc",
		HTTP:     &config.EVChargerHTTP{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "TWCcamel" {
		t.Fatalf("got %+v, want TWCcamel", got)
	}
	if !strings.Contains(got[0].Name, "1529455-02-J") {
		t.Errorf("name: %q", got[0].Name)
	}
}

func TestTeslaWCListChargersBothEndpointsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := NewTeslaWC().WithHTTPClient(srv.Client()).ListChargers(&config.EVCharger{
		Provider: "tesla-wc",
		HTTP:     &config.EVChargerHTTP{BaseURL: srv.URL},
	})
	if err == nil {
		t.Fatal("expected error when version and vitals both fail")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error should mention version: %v", err)
	}
}

func TestTeslaWCRegistered(t *testing.T) {
	p, err := Get("tesla-wc")
	if err != nil {
		t.Fatalf("Get tesla-wc: %v", err)
	}
	if p.Describe().NeedsAuth {
		t.Error("registered tesla-wc must not require auth")
	}
}
