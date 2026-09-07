package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

// ForecastLearning restarts the primary, fallback and calibration together.
// It owns the durable learning boundary; the API never edits model state.
type ForecastLearning interface {
	LearningStatus(string) forecasting.LearningStatus
	RestartLearning(context.Context, string) error
}

func (s *Server) forecastLearningStatus(signal string) forecasting.LearningStatus {
	if s.deps.ForecastLearning == nil {
		return forecasting.LearningStatus{Engine: "legacy", Status: "unavailable"}
	}
	return s.deps.ForecastLearning.LearningStatus(signal)
}

func (s *Server) handleForecastLearningReset(w http.ResponseWriter, r *http.Request, signal string) {
	if s.deps.ForecastLearning == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "forecast learning service unavailable"})
		return
	}
	if err := s.deps.ForecastLearning.RestartLearning(r.Context(), signal); err != nil {
		status := "error"
		var pending *forecasting.LearningRestartPendingError
		if errors.As(err, &pending) {
			status = "pending"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": status, "error": err.Error(), "learning": s.forecastLearningStatus(signal)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "reset", "learning": s.forecastLearningStatus(signal)})
}
