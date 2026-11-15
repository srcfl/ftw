package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// forecastLongArchiver copies the 30-day forecast tables into
// forecast-archive.db. It does not share the tracker's queue or its lock.
type forecastLongArchiver struct {
	store      *state.Store
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	startDelay time.Duration
	interval   time.Duration
	now        func() time.Time
}

func startForecastLongArchive(ctx context.Context, st *state.Store) *forecastLongArchiver {
	a := &forecastLongArchiver{store: st, startDelay: 2 * time.Minute, interval: time.Hour}
	a.Start(ctx)
	return a
}

func (a *forecastLongArchiver) Start(ctx context.Context) {
	if a == nil || a.store == nil || a.cancel != nil {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.run(workerCtx)
	}()
}

func (a *forecastLongArchiver) Stop() {
	if a == nil || a.cancel == nil {
		return
	}
	a.cancel()
	a.wg.Wait()
}

func (a *forecastLongArchiver) run(ctx context.Context) {
	a.store.ReportForecastLongArchive(state.ForecastLongArchiveStatus{
		State: "starting", CapBytes: state.ForecastLongMaxBytes,
	})
	delay := a.startDelay
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	a.runOnce(ctx)
	interval := a.interval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.runOnce(ctx)
		}
	}
}

func (a *forecastLongArchiver) runOnce(ctx context.Context) {
	if a == nil || a.store == nil || ctx.Err() != nil {
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	arch, err := a.store.OpenForecastLongArchive(openCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("forecast long archive unavailable", "err", err)
		a.store.ReportForecastLongArchive(state.ForecastLongArchiveStatus{
			State: "unavailable", CapBytes: state.ForecastLongMaxBytes, LastError: err.Error(),
		})
		return
	}
	now := time.Now().UTC()
	if a.now != nil {
		now = a.now().UTC()
	}
	if err := arch.Sync(ctx, a.store, now); err != nil && ctx.Err() == nil {
		slog.Warn("forecast long archive copy", "err", err)
	}
}
