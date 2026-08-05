package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/ocpp"
)

func getOCPPChargers(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/ocpp/chargers", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	return out
}

// OCPP off is a normal state the panel renders from, not an error.
func TestOCPPChargersDisabled(t *testing.T) {
	srv := New(&Deps{})
	out := getOCPPChargers(t, srv)
	if out["enabled"] != false {
		t.Fatalf("enabled = %v, want false", out["enabled"])
	}
	if chargers, ok := out["chargers"].([]any); !ok || len(chargers) != 0 {
		t.Fatalf("chargers = %v, want empty array", out["chargers"])
	}
}

func TestOCPPChargersReportsSnapshotSorted(t *testing.T) {
	cfg := &config.Config{OCPP: &config.OCPP{Enabled: true, PortV201: 8888}}
	srv := New(&Deps{
		Cfg:   cfg,
		CfgMu: &sync.RWMutex{},
		OCPPChargers: func() map[string]ocpp.ChargerView {
			return map[string]ocpp.ChargerView{
				"garage-right": {Online: true, Version: "2.0.1"},
				"garage-left": {
					Online: true, Connected: true, Charging: true,
					PowerW: 7400, Version: "1.6", LastAmps: 10,
					Vendor: "Charge Amps", Model: "Dawn",
				},
			}
		},
	})
	out := getOCPPChargers(t, srv)
	if out["enabled"] != true {
		t.Fatalf("enabled = %v, want true", out["enabled"])
	}
	// Unset port must surface as the default the listener actually took.
	if out["port"] != float64(8887) {
		t.Fatalf("port = %v, want 8887", out["port"])
	}
	if out["port_v201"] != float64(8888) {
		t.Fatalf("port_v201 = %v, want 8888", out["port_v201"])
	}
	chargers, ok := out["chargers"].([]any)
	if !ok || len(chargers) != 2 {
		t.Fatalf("chargers = %v, want 2 entries", out["chargers"])
	}
	first := chargers[0].(map[string]any)
	if first["id"] != "garage-left" {
		t.Fatalf("first id = %v, want garage-left (sorted)", first["id"])
	}
	if first["vendor"] != "Charge Amps" || first["model"] != "Dawn" {
		t.Fatalf("vendor/model = %v/%v", first["vendor"], first["model"])
	}
	if first["charging"] != true || first["power_w"] != float64(7400) {
		t.Fatalf("charging/power = %v/%v", first["charging"], first["power_w"])
	}
	if first["last_amps"] != float64(10) || first["version"] != "1.6" {
		t.Fatalf("last_amps/version = %v/%v", first["last_amps"], first["version"])
	}
}
