package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/selftune"
)

func TestSelfTuneStartRejectsObserveOnly(t *testing.T) {
	cfg := &config.Config{
		Drivers: []config.Driver{
			{Name: "pixii-1", ObserveOnly: true, BatteryCapacityWh: 16000},
			{Name: "pixii-2", BatteryCapacityWh: 16000},
		},
	}
	cfgMu := &sync.RWMutex{}
	modelsMu := &sync.Mutex{}
	models := map[string]*battery.Model{
		"pixii-1": battery.New("pixii-1"),
		"pixii-2": battery.New("pixii-2"),
	}
	srv := New(&Deps{
		Cfg:      cfg,
		CfgMu:    cfgMu,
		SelfTune: selftune.NewCoordinator(),
		Models:   models,
		ModelsMu: modelsMu,
		DtS:      5,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/self_tune/start",
		strings.NewReader(`{"batteries":["pixii-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("observe_only battery: status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/self_tune/start",
		strings.NewReader(`{"batteries":["pixii-2"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("controllable battery: status=%d body=%s", rr.Code, rr.Body.String())
	}
}
