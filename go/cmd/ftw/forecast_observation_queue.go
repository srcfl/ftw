package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

// Keep a bounded backlog of completed intervals. The observer owns this queue;
// issuing forecasts and scoring them must not postpone the next measurement.
const maxPendingForecastObservations = 64

type forecastObservationJob struct {
	observation forecasting.Observation
	site        forecastSite
	weather     *state.ForecastPoint
	away        bool
	archived    bool
}

func (f *forecastTracker) runObservations(ctx context.Context) {
	defer func() {
		drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		f.flushObservations(drain)
		if len(f.pendingObservations) > 0 {
			slog.Error("forecast archive: shutdown observations still pending", "intervals", len(f.pendingObservations))
		}
	}()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			f.observe(ctx)
		}
	}
}

func (f *forecastTracker) enqueueObservation(job forecastObservationJob) {
	if len(f.pendingObservations) == maxPendingForecastObservations {
		f.mu.Lock()
		f.observationOverflow = true
		f.mu.Unlock()
		slog.Error("forecast archive: observation queue full", "start_ms", job.observation.StartMS)
		return
	}
	f.pendingObservations = append(f.pendingObservations, job)
}

func (f *forecastTracker) flushObservations(ctx context.Context) {
	f.flushObservationsWith(ctx, f.store.SaveForecastObservation, f.updateObservation)
}

// Save and update retries keep the original evidence and captured features.
// A successful save followed by a failed model update skips saving on retry.
func (f *forecastTracker) flushObservationsWith(ctx context.Context,
	save func(context.Context, forecasting.Observation) error,
	update func(context.Context, forecastObservationJob) error) {
	for len(f.pendingObservations) > 0 {
		if ctx.Err() != nil {
			return
		}
		job := &f.pendingObservations[0]
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		phase := "observation_save_pending"
		var err error
		if !job.archived {
			err = save(writeCtx, job.observation)
			if err == nil {
				job.archived = true
			}
		}
		if err == nil {
			phase = "model_save_pending"
			err = update(writeCtx, *job)
		}
		if err != nil && writeCtx.Err() != nil {
			err = errors.Join(err, writeCtx.Err())
		}
		cancel()
		f.mu.Lock()
		if err != nil {
			f.observationError = phase
		} else {
			f.observationError = ""
		}
		f.mu.Unlock()
		if err != nil {
			slog.Warn("forecast archive: observation retained for retry", "phase", phase, "start_ms", job.observation.StartMS, "err", err)
			return
		}
		f.pendingObservations[0] = forecastObservationJob{}
		f.pendingObservations = f.pendingObservations[1:]
		f.requestScoring()
	}
}

func (f *forecastTracker) updateObservation(ctx context.Context, job forecastObservationJob) error {
	if f.candidate == nil {
		return nil
	}
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	f.learningMu.RLock()
	defer f.learningMu.RUnlock()
	current := f.site()
	// Old configuration evidence remains in the archive, but must not switch
	// the current model back to a previous identity when the queue recovers.
	if current.IdentityPending || current.Revision != job.site.Revision {
		return nil
	}
	site := f.learningSiteLocked(job.site)
	return f.candidate.Update(ctx, site, job.observation, job.weather, job.away)
}
