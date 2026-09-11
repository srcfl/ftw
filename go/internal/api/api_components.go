package api

import (
	"context"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/mpc"
)

type optimizerHealth interface {
	Health(context.Context) (mpc.OptimizerRuntimeInfo, error)
}

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{
		"manifest_schema_version": components.ComponentManifestSchemaVersion,
		"core":                    map[string]any{"version": s.deps.Version, "role": "safety_authority"},
		"optimizer": map[string]any{
			"configured":           false,
			"protocol_version":     components.OptimizerProtocolVersion,
			"protocol_min_version": components.OptimizerProtocolMinVersion,
		},
		"drivers": map[string]any{"host_api": components.DriverHostAPIVersion},
	}
	if worker := s.deps.MPC.ConfiguredOptimizer(); worker != nil {
		role := "champion"
		optimizer := map[string]any{
			"configured":           true,
			"role":                 role,
			"bundled_with_core":    s.deps.MPC.OptimizerBundledWithCore(),
			"protocol_version":     components.OptimizerProtocolVersion,
			"protocol_min_version": components.OptimizerProtocolMinVersion,
		}
		if health, ok := worker.(optimizerHealth); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			info, err := health.Health(ctx)
			cancel()
			if err != nil {
				optimizer["healthy"] = false
				optimizer["degraded"] = true
				optimizer["error"] = err.Error()
				optimizer["health_error"] = err.Error()
			} else {
				optimizer["healthy"] = true
				optimizer["runtime"] = info
			}
		}
		applyLatestOptimizerPlanStatus(optimizer, s.deps.MPC.Latest())
		result["optimizer"] = optimizer
	}
	if s.deps.DriverRepository != nil {
		result["drivers"] = s.deps.DriverRepository.Status()
	}
	if s.deps.Bundle != nil {
		result["bundle"] = s.deps.Bundle
	}
	if s.deps.SelfUpdate != nil {
		result["updates"] = map[string]any{
			"release": s.deps.SelfUpdate.Info(),
			"status":  s.deps.SelfUpdate.Status(),
		}
	}
	writeJSON(w, 200, result)
}

func applyLatestOptimizerPlanStatus(status map[string]any, plan *mpc.Plan) {
	if plan == nil || plan.Solver == nil {
		return
	}
	status["active_solver"] = plan.Solver
	status["last_plan_at_ms"] = plan.GeneratedAtMs
	// Core producing the plan is the default arrangement, not a degradation.
	// Only the fallback flag — set when the operator asked for the external
	// planner and did not get it — means the optimizer is failing.
	if !plan.Solver.Fallback {
		return
	}
	status["healthy"] = false
	status["degraded"] = true
	reason := plan.Solver.FallbackReason
	if reason == "" {
		reason = "primary optimizer did not produce the active plan"
	}
	status["fallback_reason"] = reason
}
