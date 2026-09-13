package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func (f *forecastTracker) requestScoring() {
	select {
	case f.scoreWake <- struct{}{}:
	default:
	}
}

// Scoring has one worker and one coalesced wake. It cannot block sampling or
// candidate learning. On restart, replay retained observations to recover gaps;
// saved scores are idempotent and the original evidence stays immutable.
func (f *forecastTracker) runScoring(ctx context.Context) {
	since := f.now().Add(-state.ForecastIssueRetention)
	f.requestScoring()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.scoreWake:
			now := f.now()
			if err := f.scoreRange(ctx, since, now); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("forecast evaluation incomplete; saved pages retained", "err", err)
				continue
			}
			// Revisit the overlap for delayed hourly observations and issue writes.
			since = now.Add(-2 * time.Hour)
			f.refreshEvidence(ctx, f.now())
		}
	}
}

func (f *forecastTracker) scoreRange(ctx context.Context, since, now time.Time) error {
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	observations, err := f.store.LoadForecastObservations(readCtx, since.UnixMilli(), now.UnixMilli())
	cancel()
	if err != nil {
		return err
	}
	if len(observations) == 0 {
		return nil
	}
	const pageSize = 4
	cursor := state.ForecastIssueCursor{}
	for {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		issues, next, err := f.store.LoadForecastScorePage(readCtx, since.Add(-48*time.Hour).UnixMilli(), now.UnixMilli(), cursor, pageSize)
		cancel()
		if err != nil {
			return err
		}
		if len(issues) == 0 {
			return nil
		}
		scores := forecasting.Errors(issues, observations, now.UnixMilli())
		if len(scores) > 0 {
			writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = f.store.SaveForecastErrors(writeCtx, scores, now.UnixMilli())
			cancel()
			if err != nil {
				return err
			}
		}
		cursor = next
		if len(issues) < pageSize {
			return nil
		}
	}
}
