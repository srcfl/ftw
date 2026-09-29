package main

import (
	"context"
	"fmt"
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

// One worker owns the pass and its cursor. Retry a failed page without replaying
// committed pages. On process restart, replay the retained immutable evidence.
func (f *forecastTracker) runScoring(ctx context.Context) {
	since := f.now().Add(-state.ForecastIssueRetention)
	var pass *forecastScorePass
	var rescore bool
	retry := time.NewTicker(time.Minute)
	defer retry.Stop()
	f.requestScoring()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			if pass == nil {
				continue
			}
		case <-f.scoreWake:
			rescore = pass != nil
		}
		if pass == nil {
			pass = newForecastScorePass(since, f.now())
		}
		err := pass.run(ctx, f.store)
		if ctx.Err() != nil {
			return
		}
		// Even a later page or maintenance failure must not hide earlier commits
		// from calibration. Reads have their own budget and retain the old cache
		// on failure.
		f.refreshEvidence(ctx, f.now())
		if err != nil {
			slog.Warn("forecast evaluation incomplete; will resume", "err", err,
				"pages", pass.pages, "cursor_ms", pass.cursor.IssuedAtMS,
				"cursor_id", pass.cursor.ID, "through_ms", pass.until.UnixMilli())
			continue
		}
		slog.Info("forecast evaluation complete", "pages", pass.pages, "through_ms", pass.until.UnixMilli())
		// Revisit the overlap for delayed hourly observations and issue writes.
		// Use the completed cutoff, even when a retry finished much later.
		since = pass.until.Add(-2 * time.Hour)
		// A wake used to resume an old pass also requested fresh evidence.
		if rescore {
			f.requestScoring()
			rescore = false
		}
		pass = nil
	}
}

type forecastScoreStore interface {
	LoadForecastObservations(context.Context, int64, int64) ([]forecasting.Observation, error)
	LoadForecastScorePage(context.Context, int64, int64, state.ForecastIssueCursor, int) ([]forecasting.Issue, state.ForecastIssueCursor, error)
	SaveForecastErrors(context.Context, []forecasting.ErrorSample, int64) error
	PruneForecastErrors(context.Context, int64) error
}

type forecastScorePass struct {
	since, until time.Time
	cursor       state.ForecastIssueCursor
	observations []forecasting.Observation
	loaded       bool
	maintenance  bool
	done         bool
	pages        int
}

func newForecastScorePass(since, until time.Time) *forecastScorePass {
	// Clean up any page committed before a process restart as well.
	return &forecastScorePass{since: since, until: until, maintenance: true}
}

func (p *forecastScorePass) run(ctx context.Context, store forecastScoreStore) error {
	if !p.loaded {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		observations, err := store.LoadForecastObservations(readCtx, p.since.UnixMilli(), p.until.UnixMilli())
		cancel()
		if err != nil {
			return fmt.Errorf("load observations: %w", err)
		}
		// Freeze the evidence and cutoff across retries. New observations belong
		// to the next overlapping pass, including those arriving during a retry.
		p.observations, p.loaded = observations, true
		p.done = len(observations) == 0
	}
	const pageSize = 4
	for {
		if p.maintenance {
			pruneCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := store.PruneForecastErrors(pruneCtx, p.until.UnixMilli())
			cancel()
			if err != nil {
				return fmt.Errorf("prune committed scores: %w", err)
			}
			p.maintenance = false
		}
		if p.done {
			return nil
		}
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		issues, next, err := store.LoadForecastScorePage(readCtx, p.since.Add(-48*time.Hour).UnixMilli(), p.until.UnixMilli(), p.cursor, pageSize)
		cancel()
		if err != nil {
			return fmt.Errorf("load issue page: %w", err)
		}
		if len(issues) == 0 {
			p.done = true
			return nil
		}
		scores := forecasting.Errors(issues, p.observations, p.until.UnixMilli())
		if len(scores) > 0 {
			writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = store.SaveForecastErrors(writeCtx, scores, p.until.UnixMilli())
			cancel()
			if err != nil {
				return fmt.Errorf("save score page: %w", err)
			}
			p.maintenance = true
		}
		p.cursor = next
		p.pages++
		p.done = len(issues) < pageSize
	}
}
