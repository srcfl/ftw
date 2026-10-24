package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// The app sends Charge now with the phases the API reports. A charger whose
// table stops below 6 A on three phases runs on one, so it reports one
// unless pinned to 3p; a loadpoint that can run three reports three.
func TestLoadpointsReportTheChargersPhases(t *testing.T) {
	onePhase := []float64{0, 1380, 2300, 3680}
	mixed := []float64{0, 1380, 2300, 3680, 4140, 6900, 11040}
	for _, tc := range []struct {
		name  string
		mode  string
		steps []float64
		maxW  float64
		want  int
	}{
		{"32 A one-phase table, auto", "auto", onePhase, 7360, 1},
		{"one-phase table, unset", "", onePhase, 7360, 1},
		{"one-phase table pinned 3p", "3p", onePhase, 7360, 3},
		{"mixed table, auto", "auto", mixed, 11040, 3},
		{"no table, unset", "", nil, 11000, 3},
		{"pinned 1p", "1p", mixed, 11040, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := loadpoint.NewManager()
			mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", PhaseMode: tc.mode, AllowedStepsW: tc.steps,
				MinChargeW: 1380, MaxChargeW: tc.maxW}})
			cfg := &config.Config{Fuse: config.Fuse{MaxAmps: 25, Phases: 3, Voltage: 230}}
			srv := New(&Deps{Loadpoints: mgr, Cfg: cfg, CfgMu: &sync.RWMutex{}})
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
			var body struct {
				Loadpoints []loadpoint.State `json:"loadpoints"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Loadpoints) != 1 {
				t.Fatalf("GET /api/loadpoints = %d: %s", rr.Code, rr.Body.String())
			}
			if got := body.Loadpoints[0].Phases; got != tc.want {
				t.Fatalf("phases %d, want %d", got, tc.want)
			}
		})
	}
}
