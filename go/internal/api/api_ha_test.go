package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func homeAssistantStatus(t *testing.T, deps *Deps) map[string]any {
	t.Helper()
	srv := New(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/ha/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return body
}

func TestHAStatusReportsDisabledFromConfig(t *testing.T) {
	body := homeAssistantStatus(t, &Deps{
		Version: "test",
		CfgMu:   &sync.RWMutex{},
		Cfg: &config.Config{HomeAssistant: &config.HomeAssistant{
			Enabled: false,
			Broker:  "192.168.1.65",
			Port:    1883,
		}},
	})
	if body["enabled"] != false {
		t.Fatalf("disabled config reported enabled=%v", body["enabled"])
	}
}

func TestHAStatusKeepsEnabledWhenBridgeFailedToStart(t *testing.T) {
	body := homeAssistantStatus(t, &Deps{
		Version: "test",
		CfgMu:   &sync.RWMutex{},
		Cfg: &config.Config{HomeAssistant: &config.HomeAssistant{
			Enabled: true,
			Broker:  "192.168.1.65",
			Port:    1883,
		}},
	})
	if body["enabled"] != true {
		t.Fatalf("enabled config reported enabled=%v", body["enabled"])
	}
	if body["connected"] != false {
		t.Fatalf("missing bridge reported connected=%v", body["connected"])
	}
	if body["broker"] != "192.168.1.65:1883" {
		t.Fatalf("broker = %v", body["broker"])
	}
}
