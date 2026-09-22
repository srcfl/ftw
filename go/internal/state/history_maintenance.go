package state

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

type HistoryMaintenanceStatus struct {
	State            string                                `json:"state"`
	Phase            string                                `json:"phase,omitempty"`
	StartedMS        int64                                 `json:"started_ms,omitempty"`
	LastSuccessMS    int64                                 `json:"last_success_ms,omitempty"`
	LastError        string                                `json:"last_error,omitempty"`
	LastFailureMS    int64                                 `json:"last_failure_ms,omitempty"`
	LastFailureError string                                `json:"last_failure_error,omitempty"`
	Runs             uint64                                `json:"runs"`
	Failures         uint64                                `json:"failures"`
	File             string                                `json:"file,omitempty"`
	Operation        string                                `json:"operation,omitempty"`
	RowsDone         int64                                 `json:"rows_done"`
	RowsTotal        int64                                 `json:"rows_total,omitempty"`
	UpdatedMS        int64                                 `json:"updated_ms,omitempty"`
	Work             map[string]HistoryMaintenanceProgress `json:"work,omitempty"`
}

type HistoryMaintenanceProgress struct {
	File      string `json:"file,omitempty"`
	Operation string `json:"operation,omitempty"`
	RowsDone  int64  `json:"rows_done"`
	RowsTotal int64  `json:"rows_total,omitempty"`
	UpdatedMS int64  `json:"updated_ms,omitempty"`
}

// PauseHistoryMaintenance stops archive work before a full backup captures
// its database and files. Live telemetry and durable user goals keep writing.
func (s *Store) PauseHistoryMaintenance(ctx context.Context) (func(), error) {
	s.maintenanceStatusMu.Lock()
	s.maintenancePaused++
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
	}
	s.maintenanceStatusMu.Unlock()
	resume := func() { s.maintenanceStatusMu.Lock(); s.maintenancePaused--; s.maintenanceStatusMu.Unlock() }
	if err := lockContext(ctx, s.maintenanceRunMu.TryLock); err != nil {
		resume()
		return nil, err
	}
	s.maintenanceRunMu.Unlock()
	return resume, nil
}

func (s *Store) HistoryMaintenanceStatus() HistoryMaintenanceStatus {
	s.maintenanceStatusMu.Lock()
	defer s.maintenanceStatusMu.Unlock()
	status := s.maintenanceStatus
	status.Work = maps.Clone(status.Work)
	if status.State == "" {
		status.State = "not_started"
	}
	return status
}

// historyStageBudget bounds one non-archive maintenance stage. Archive stages
// carry historyArchiveTurn and yield at a committed cursor. A test shrinks
// the budget so a blocked stage can yield without waiting half a minute.
var historyStageBudget = 30 * time.Second

var historyArchiveTurn = 30 * time.Second

// historyStageForTest, when set, may replace one stage body. Production
// leaves it nil. Tests restore the previous value before returning.
var historyStageForTest func(name string, ctx context.Context) (replaced bool, err error)

// MaintainHistory runs the existing retention work and records its result.
// The caller serializes it with backup/restore. Archives yield at committed
// cursors; other stages have a deadline. A spent budget stays pending so the
// caller can resume; real failures remain visible.
func (s *Store) MaintainHistory(parent context.Context, coldDir string, days int, now time.Time) error {
	if !s.maintenanceRunMu.TryLock() {
		if err := lockContext(parent, s.maintenanceRunMu.TryLock); err != nil {
			return err
		}
	}
	defer s.maintenanceRunMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.maintenanceStatusMu.Lock()
	if s.maintenancePaused > 0 {
		s.maintenanceStatus.State = "paused"
		s.maintenanceStatusMu.Unlock()
		return nil
	}
	s.maintenanceCancel = cancel
	s.maintenanceStatus.State, s.maintenanceStatus.StartedMS = "running", time.Now().UnixMilli()
	s.maintenanceStatus.Runs++
	s.maintenanceStatusMu.Unlock()
	var failures []error
	pending := false
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
		s.maintenanceStatus.File, s.maintenanceStatus.Operation = "", ""
		s.maintenanceStatus.RowsDone, s.maintenanceStatus.RowsTotal = 0, 0
		s.maintenanceStatusMu.Unlock()
		if err := ctx.Err(); err != nil {
			if parent.Err() == nil {
				pending = true
				break
			}
			failures = append(failures, err)
			break
		}
		parentCtx := ctx
		stop := func() {}
		switch stage.name {
		case "aggregate_archive", "sample_archive", "legacy_compaction":
			ctx = context.WithValue(parentCtx, archiveTurnKey{}, time.Now().Add(historyArchiveTurn))
		default:
			ctx, stop = context.WithTimeout(parentCtx, historyStageBudget)
		}
		var err error
		if historyStageForTest != nil {
			var replaced bool
			replaced, err = historyStageForTest(stage.name, ctx)
			if !replaced {
				err = stage.run()
			}
		} else {
			err = stage.run()
		}
		// stop cancels the stage budget. Read it first, or a deadline looks
		// like a live parent and is stored as a hard failure.
		stageErr := ctx.Err()
		stop()
		ctx = parentCtx
		if errors.Is(err, errArchiveTurnComplete) {
			pending = true
			continue
		}
		if err != nil && parent.Err() == nil && maintenanceBudgetSpent(stage.name, stageErr, err) {
			pending = true
			break
		}
		if err != nil {
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
	s.maintenanceCancel = nil
	if err == nil && pending {
		s.maintenanceStatus.State, s.maintenanceStatus.LastError = "pending", ""
	} else if err == nil {
		s.maintenanceStatus.State, s.maintenanceStatus.LastError = "complete", ""
		s.maintenanceStatus.LastSuccessMS = time.Now().UnixMilli()
	} else {
		s.maintenanceStatus.State, s.maintenanceStatus.LastError = "failed", err.Error()
		s.maintenanceStatus.Failures++
		s.maintenanceStatus.LastFailureMS, s.maintenanceStatus.LastFailureError = time.Now().UnixMilli(), err.Error()
	}
	return err
}

// maintenanceBudgetSpent reports that this stage stopped because its time ran
// out while the caller is still running. A dashboard bucket that cannot commit
// inside its own write budget keeps a live stage context and a wrapped
// deadline, and stays a real failure.
func maintenanceBudgetSpent(stage string, stageErr, err error) bool {
	if stageErr != nil {
		return true
	}
	switch stage {
	case "aggregate_archive", "sample_archive", "legacy_compaction":
		return errors.Is(err, context.DeadlineExceeded)
	default:
		return false
	}
}
