package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/mpc"
)

type componentTestOptimizer struct{ healthErr error }

func (o *componentTestOptimizer) Optimize(context.Context, []mpc.Slot, mpc.Params) (mpc.Plan, error) {
	return mpc.Plan{}, errors.New("not used")
}
func (o *componentTestOptimizer) Close() error { return nil }
func (o *componentTestOptimizer) Health(context.Context) (mpc.OptimizerRuntimeInfo, error) {
	return mpc.OptimizerRuntimeInfo{}, o.healthErr
}

func TestApplyLatestOptimizerPlanStatusMarksFallbackDegraded(t *testing.T) {
	status := map[string]any{"healthy": true}
	plan := &mpc.Plan{
		GeneratedAtMs: 1234,
		Solver: &mpc.SolverInfo{
			Engine: "go-dp", Status: "fallback", Fallback: true,
			FallbackReason: `start optimizer "python3": executable file not found`,
		},
	}
	applyLatestOptimizerPlanStatus(status, plan)
	if status["healthy"] != false || status["degraded"] != true {
		t.Fatalf("fallback status = %#v, want unhealthy degraded optimizer", status)
	}
	if status["fallback_reason"] != plan.Solver.FallbackReason || status["last_plan_at_ms"] != plan.GeneratedAtMs {
		t.Fatalf("fallback detail = %#v, want reason and plan time", status)
	}
	if status["active_solver"] != plan.Solver {
		t.Fatalf("active_solver = %#v, want latest solver", status["active_solver"])
	}
}

func TestComponentsReportsWorkerHealthFailure(t *testing.T) {
	svc := &mpc.Service{Optimizer: &componentTestOptimizer{healthErr: errors.New("worker unavailable")}}
	srv := New(&Deps{MPC: svc})
	req := httptest.NewRequest(http.MethodGet, "/api/components", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Optimizer struct {
			Healthy     bool   `json:"healthy"`
			Degraded    bool   `json:"degraded"`
			HealthError string `json:"health_error"`
		} `json:"optimizer"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Optimizer.Healthy || !body.Optimizer.Degraded || body.Optimizer.HealthError != "worker unavailable" {
		t.Fatalf("optimizer status = %+v", body.Optimizer)
	}
}
