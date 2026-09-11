package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

// TestMPCDiagnoseCarriesCoreSolverIdentity pins the JSON /api/mpc/diagnose
// serves for a Core-planned replan. The diagnostic blobs are what the soak
// analyses offline, so an empty or missing "solver" object would leave every
// snapshot unattributable — which engine, at which grid, in how long.
func TestMPCDiagnoseCarriesCoreSolverIdentity(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := mpc.New(st, nil, "SE3", mpc.Params{
		Mode: mpc.ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500
	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("no plan")
	}

	srv := New(&Deps{MPC: svc, State: st})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mpc/diagnose", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Enabled    bool `json:"enabled"`
		Diagnostic *struct {
			Solver *struct {
				Engine       string  `json:"engine"`
				Backend      string  `json:"backend"`
				Status       string  `json:"status"`
				SoCLevels    int     `json:"soc_levels"`
				ActionLevels int     `json:"action_levels"`
				SolveMs      float64 `json:"solve_ms"`
				Fallback     bool    `json:"fallback"`
			} `json:"solver"`
		} `json:"diagnostic"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	if !body.Enabled || body.Diagnostic == nil || body.Diagnostic.Solver == nil {
		t.Fatalf("diagnostic solver absent: %s", rr.Body.String())
	}
	s := body.Diagnostic.Solver
	if s.Engine != "core" || s.Backend != "dp" || s.Status != "optimal" {
		t.Fatalf("solver identity = %+v", s)
	}
	if s.SoCLevels != 11 || s.ActionLevels != 5 {
		t.Fatalf("solver grid = %dx%d, want 11x5", s.SoCLevels, s.ActionLevels)
	}
	if s.SolveMs <= 0 || s.Fallback {
		t.Fatalf("core plan reported as an unmeasured fallback: %+v", s)
	}
}
