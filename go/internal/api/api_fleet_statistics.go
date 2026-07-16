package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/fleetstats"
)

func (s *Server) handleFleetStatisticsPreview(w http.ResponseWriter, r *http.Request) {
	if s.deps.FleetStats == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fleet statistics unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	payload, err := s.deps.FleetStats.Preview(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.deps.FleetStats.Enabled(),
		"payload": payload,
	})
}

func (s *Server) handleFleetStatisticsSubmit(w http.ResponseWriter, r *http.Request) {
	if s.deps.FleetStats == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fleet statistics unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	payload, err := s.deps.FleetStats.Submit(ctx)
	if errors.Is(err, fleetstats.ErrDisabled) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "submitted", "payload": payload})
}
