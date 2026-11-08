package mpc

import (
	"context"
	"os"
	"testing"
	"time"
)

// A worker that takes hourly_import_caps plans the 25 October field case
// itself: Core sends the hours, and the worker's plan keeps every hour and
// shows the goal short. The bundled worker skips until its pin takes them.
func TestNativeEnergyplanTakesHourlyImportCaps(t *testing.T) {
	binary := os.Getenv("FTW_NATIVE_SOLVER")
	if binary == "" {
		t.Skip("run make native-solver-test to include the Rust worker")
	}
	o, err := NewEnergyplanOptimizer(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := o.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !optimizerHasFeature(info, "hourly_import_caps") {
		t.Skip("this worker does not take hourly_import_caps")
	}
	svc, start := importCeilingFieldService(t, o, true)
	plan := checkImportCeilingFieldPlan(t, svc, start)
	if plan.Solver == nil || plan.Solver.Fallback {
		t.Fatalf("solver %+v; want the worker's own plan", plan.Solver)
	}
}
