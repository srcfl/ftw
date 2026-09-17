package state

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type HistoryMaintenanceStatus struct {
	State            string `json:"state"`
	Phase            string `json:"phase,omitempty"`
	StartedMS        int64  `json:"started_ms,omitempty"`
	LastSuccessMS    int64  `json:"last_success_ms,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	LastFailureMS    int64  `json:"last_failure_ms,omitempty"`
	LastFailureError string `json:"last_failure_error,omitempty"`
	Runs             uint64 `json:"runs"`
	Failures         uint64 `json:"failures"`
}

func (s *Store) HistoryMaintenanceStatus() HistoryMaintenanceStatus {
	s.maintenanceStatusMu.Lock()
	defer s.maintenanceStatusMu.Unlock()
	status := s.maintenanceStatus
	if status.State == "" {
		status.State = "not_started"
	}
	return status
}

// MaintainHistory runs the existing retention work and records its result.
// The caller serializes it with backup/restore. Each stage must yield to live
// writes; the overall deadline also bounds a permanently stalled archive.
func (s *Store) MaintainHistory(parent context.Context, coldDir string, days int, now time.Time) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Hour)
	defer cancel()
	s.maintenanceStatusMu.Lock()
	s.maintenanceStatus.State, s.maintenanceStatus.StartedMS = "running", time.Now().UnixMilli()
	s.maintenanceStatus.Runs++
	s.maintenanceStatusMu.Unlock()
	var failures []error
	for _, stage := range []struct {
		name string
		run  func() error
	}{
		{"dashboard_rollup", func() error { return s.Prune(ctx) }},
		{"energy_rollup", func() error { _, _, err := s.PruneEnergyLedger(ctx, now); return err }},
		{"diagnostic_archive", func() error { _, _, err := s.RolloffDiagnosticsToParquet(ctx, coldDir); return err }},
		{"aggregate_archive", func() error { return s.MaintainAggregateHistory(ctx, coldDir, now) }},
		{"sample_archive", func() error {
			if s.aggregateHistory.Load() {
				_, _, err := s.rolloffSamples(ctx, coldDir, AggregateRecentRetention)
				return err
			}
			return s.PruneHistorySamples(ctx, days, now)
		}},
		{"legacy_compaction", func() error {
			if s.aggregateHistory.Load() {
				return s.CompactLegacyHistory(ctx, coldDir, now)
			}
			return nil
		}},
		{"diagnostic_retention", func() error {
			retention := days
			if s.aggregateHistory.Load() {
				retention = 30
			}
			_, err := PruneDiagnosticsParquet(coldDir, retention, now)
			return err
		}},
	} {
		s.maintenanceStatusMu.Lock()
		s.maintenanceStatus.Phase = stage.name
		s.maintenanceStatusMu.Unlock()
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := stage.run(); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", stage.name, err))
			// A later archive stage may run for a long time. Report this
			// failure now, rather than hiding it until the full cycle ends.
			s.maintenanceStatusMu.Lock()
			s.maintenanceStatus.LastError = errors.Join(failures...).Error()
			s.maintenanceStatus.LastFailureMS = time.Now().UnixMilli()
			s.maintenanceStatus.LastFailureError = s.maintenanceStatus.LastError
			s.maintenanceStatusMu.Unlock()
		}
	}
	err := errors.Join(failures...)
	s.maintenanceStatusMu.Lock()
	defer s.maintenanceStatusMu.Unlock()
	s.maintenanceStatus.Phase = ""
	if err == nil {
		s.maintenanceStatus.State, s.maintenanceStatus.LastError = "complete", ""
		s.maintenanceStatus.LastSuccessMS = time.Now().UnixMilli()
	} else {
		s.maintenanceStatus.State, s.maintenanceStatus.LastError = "failed", err.Error()
		s.maintenanceStatus.Failures++
		s.maintenanceStatus.LastFailureMS, s.maintenanceStatus.LastFailureError = time.Now().UnixMilli(), err.Error()
	}
	return err
}
