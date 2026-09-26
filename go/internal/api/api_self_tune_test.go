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
	// Capacities is the controllable pool as cmd/ftw builds it: observe_only
	// drivers are left out.
	srv := New(&Deps{
		Cfg:        cfg,
		CfgMu:      cfgMu,
		CapMu:      &sync.RWMutex{},
		Capacities: map[string]float64{"pixii-2": 16000},
		SelfTune:   selftune.NewCoordinator(),
		Models:     models,
		ModelsMu:   modelsMu,
		DtS:        5,
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

// A run steps each named battery for about 166 s while normal control is
// paused. Only the controllable pool may be named, and each battery once: a
// charger or a telemetry-only battery cannot take the step, and a repeated
// name only extends the pause.
func TestSelfTuneStartAcceptsOnlyControllableBatteriesOnce(t *testing.T) {
	cfg := &config.Config{
		Drivers: []config.Driver{
			{Name: "bat", BatteryCapacityWh: 10000},
			{Name: "easee"},
			{Name: "zap", BatteryTelemetryOnly: true, BatteryCapacityWh: 10000},
		},
	}
	coordinator := selftune.NewCoordinator()
	srv := New(&Deps{
		Cfg:        cfg,
		CfgMu:      &sync.RWMutex{},
		CapMu:      &sync.RWMutex{},
		Capacities: map[string]float64{"bat": 10000},
		SelfTune:   coordinator,
		Models:     map[string]*battery.Model{"bat": battery.New("bat")},
		ModelsMu:   &sync.Mutex{},
		DtS:        5,
	})
	start := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/self_tune/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	for _, body := range []string{
		`{"batteries":["easee"]}`,
		`{"batteries":["zap"]}`,
		`{"batteries":["missing"]}`,
		`{"batteries":["bat","bat"]}`,
		`{"batteries":["bat","easee"]}`,
	} {
		if rr := start(body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", body, rr.Code, rr.Body.String())
		}
		if coordinator.Status().Active {
			t.Fatalf("%s: a refused request started a self-tune run", body)
		}
	}

	if rr := start(`{"batteries":["bat"]}`); rr.Code != http.StatusOK {
		t.Fatalf("controllable battery: status=%d body=%s", rr.Code, rr.Body.String())
	}
	coordinator.Cancel()
}
