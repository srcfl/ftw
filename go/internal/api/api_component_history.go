package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srcfl/ftw/go/internal/selfupdate"
	"github.com/srcfl/ftw/go/internal/state"
)

func (s *Server) handleComponentHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 503, map[string]string{"error": "component history unavailable"})
		return
	}
	if s.deps.SelfUpdate != nil {
		s.recordComponentStatus(s.deps.SelfUpdate.Status(), "")
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	events, err := s.deps.State.ComponentUpdates(r.URL.Query().Get("kind"), r.URL.Query().Get("id"), limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

func (s *Server) recordComponentStatus(status selfupdate.UpdateStatus, fromVersion string) {
	if s.deps.State == nil || status.Action == "" || status.StartedAt.IsZero() {
		return
	}
	component := status.Component
	if component == "" {
		component = "core"
	}
	outcome := "in_progress"
	finished := int64(0)
	switch status.State {
	case "done":
		outcome = "succeeded"
		finished = status.UpdatedAt.UnixMilli()
	case "failed":
		outcome = "failed"
		finished = status.UpdatedAt.UnixMilli()
	}
	event := state.ComponentUpdate{
		OperationKey: componentOperationKey(component, status.Action, status.Target, status.StartedAt),
		Kind:         component, ComponentID: component, Action: status.Action,
		FromVersion: fromVersion, ToVersion: status.Target, Outcome: outcome,
		Message: status.Message, StartedAtMS: status.StartedAt.UnixMilli(), FinishedAtMS: finished,
	}
	if _, err := s.deps.State.UpsertComponentUpdate(event); err != nil {
		slog.Warn("component history: record status", "component", component, "action", status.Action, "err", err)
	}
}

func (s *Server) recordDriverUpdate(driverID, action, fromVersion, toVersion, outcome, message string, started time.Time) {
	if s.deps.State == nil {
		return
	}
	finished := int64(0)
	if outcome != "in_progress" {
		finished = time.Now().UnixMilli()
	}
	event := state.ComponentUpdate{
		OperationKey: componentOperationKey("driver:"+driverID, action, toVersion, started),
		Kind:         "driver", ComponentID: driverID, Action: action,
		FromVersion: fromVersion, ToVersion: toVersion, Outcome: outcome, Message: message,
		StartedAtMS: started.UnixMilli(), FinishedAtMS: finished,
	}
	if _, err := s.deps.State.UpsertComponentUpdate(event); err != nil {
		slog.Warn("component history: record driver", "driver", driverID, "action", action, "err", err)
	}
}

func componentOperationKey(component, action, target string, started time.Time) string {
	return fmt.Sprintf("%s:%d:%s:%s", component, started.UnixMilli(), action, target)
}
