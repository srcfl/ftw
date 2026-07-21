package api

import (
	"context"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/selfupdate"
)

type optimizerHealth interface {
	Health(context.Context) (mpc.OptimizerRuntimeInfo, error)
}

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{
		"manifest_schema_version": components.ComponentManifestSchemaVersion,
		"core":                    map[string]any{"version": s.deps.Version, "role": "safety_authority"},
		"optimizer":               map[string]any{"configured": false, "protocol_version": components.OptimizerProtocolVersion},
		"drivers":                 map[string]any{"host_api": components.DriverHostAPIVersion},
	}
	if s.deps.MPC != nil && s.deps.MPC.Optimizer != nil {
		optimizer := map[string]any{"configured": true, "protocol_version": components.OptimizerProtocolVersion}
		if health, ok := s.deps.MPC.Optimizer.(optimizerHealth); ok {
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
				if s.deps.OptimizerUpdate != nil {
					s.deps.OptimizerUpdate.SetCurrentVersion(info.Version)
				}
			}
		}
		applyLatestOptimizerPlanStatus(optimizer, s.deps.MPC.Latest())
		if s.deps.OptimizerUpdate != nil {
			if r.URL.Query().Get("force") == "1" {
				if info, err := s.deps.OptimizerUpdate.Check(r.Context(), true); err != nil {
					info.Err = err.Error()
					optimizer["updates"] = info
				} else {
					optimizer["updates"] = info
				}
			} else {
				optimizer["updates"] = s.deps.OptimizerUpdate.Info()
			}
		}
		result["optimizer"] = optimizer
	}
	if s.deps.DriverRepository != nil {
		result["drivers"] = s.deps.DriverRepository.Status()
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
	if !plan.Solver.Fallback && plan.Solver.Engine != "go-dp" {
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

func (s *Server) handleOptimizerComponentUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil || s.deps.OptimizerUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	info := s.deps.OptimizerUpdate.Info()
	if !info.SidecarReady {
		writeJSON(w, 502, map[string]string{"error": "updater sidecar not ready"})
		return
	}
	var body struct {
		Target string `json:"target,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if body.Target == "" {
		body.Target = info.Latest
	}
	if body.Target == "" {
		body.Target = info.Current
	}
	if body.Target == "" || body.Target == "dev" {
		writeJSON(w, 409, map[string]string{"error": "no immutable optimizer target available"})
		return
	}
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "component update already in progress"})
		return
	}
	started := time.Now()
	status := selfupdate.UpdateStatus{
		State: "starting", Action: "update", Component: "optimizer", Target: body.Target,
		StartedAt: started, UpdatedAt: started, Message: "starting optimizer update",
	}
	s.writeVersionUpdateStatus(status)
	s.recordComponentStatus(status, s.optimizerCurrentVersion(r.Context()))
	go func(target string) {
		defer s.versionUpdateMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.deps.SelfUpdate.TriggerComponentAt(ctx, "update", target, "optimizer", started); err != nil {
			s.writeVersionUpdateStatus(selfupdate.UpdateStatus{
				State: "failed", Action: "update", Component: "optimizer", Target: target,
				StartedAt: started, UpdatedAt: time.Now(), Message: err.Error(),
			})
		}
	}(body.Target)
	writeJSON(w, 202, map[string]any{"status": "started", "component": "optimizer", "target": body.Target})
}

func (s *Server) handleOptimizerComponentChannel(w http.ResponseWriter, r *http.Request) {
	if s.deps.OptimizerUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "optimizer updates disabled"})
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	channel, err := selfupdate.ParseChannel(body.Channel)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := s.deps.OptimizerUpdate.SetChannel(channel); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	info, err := s.deps.OptimizerUpdate.Check(r.Context(), true)
	if err != nil {
		info.Err = err.Error()
		writeJSON(w, 502, info)
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) handleOptimizerComponentRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil || !s.deps.SelfUpdate.Info().SidecarReady {
		writeJSON(w, 503, map[string]string{"error": "updater sidecar not ready"})
		return
	}
	status := s.deps.SelfUpdate.Status()
	previousImageID := status.PreviousImages["optimizer"]
	if previousImageID == "" && status.Component == "optimizer" {
		previousImageID = status.PreviousImageID
	}
	if previousImageID == "" {
		writeJSON(w, 409, map[string]string{"error": "no previous optimizer image is available"})
		return
	}
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "component update already in progress"})
		return
	}
	started := time.Now()
	statusEvent := selfupdate.UpdateStatus{
		State: "starting", Action: "component_rollback", Component: "optimizer",
		StartedAt: started, UpdatedAt: started, Message: "starting optimizer rollback",
		PreviousImageID: previousImageID, PreviousImages: status.PreviousImages,
	}
	s.writeVersionUpdateStatus(statusEvent)
	s.recordComponentStatus(statusEvent, s.optimizerCurrentVersion(r.Context()))
	go func() {
		defer s.versionUpdateMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.deps.SelfUpdate.TriggerComponentAt(ctx, "component_rollback", "", "optimizer", started); err != nil {
			s.writeVersionUpdateStatus(selfupdate.UpdateStatus{
				State: "failed", Action: "component_rollback", Component: "optimizer",
				StartedAt: started, UpdatedAt: time.Now(), Message: err.Error(), PreviousImageID: previousImageID,
				PreviousImages: status.PreviousImages,
			})
		}
	}()
	writeJSON(w, 202, map[string]any{"status": "started", "component": "optimizer", "action": "rollback"})
}

func (s *Server) optimizerCurrentVersion(ctx context.Context) string {
	if s.deps.MPC == nil || s.deps.MPC.Optimizer == nil {
		return ""
	}
	health, ok := s.deps.MPC.Optimizer.(optimizerHealth)
	if !ok {
		return ""
	}
	healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runtime, err := health.Health(healthCtx)
	if err != nil {
		return ""
	}
	return runtime.Version
}
