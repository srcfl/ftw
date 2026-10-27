package drivers

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// A driver whose start fails (a serial node not yet enumerated, a device
// still booting after a power cut) is retried with this backoff until it
// starts, leaves the config, or the registry shuts down. Running drivers are
// never torn down here.
const (
	spawnRetryMinDelay = 10 * time.Second
	spawnRetryMaxDelay = 5 * time.Minute
)

// spawnRetry is one pending retry. The map entry is the claim: an Add,
// Restart, Reload or Remove for the same name replaces or deletes it under the
// lifecycle gate, so a timer that fires late finds a different entry and stops.
type spawnRetry struct {
	cfg        config.Driver
	delay      time.Duration
	attempting bool
	timer      *time.Timer
}

// takeSpawnRetry removes the pending retry for name and returns the delay the
// next failure should double, or 0 when this attempt did not come from the
// retry timer. The caller holds the lifecycle gate for name.
func (r *Registry) takeSpawnRetry(name string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.spawnRetries[name]
	if e == nil {
		return 0
	}
	delete(r.spawnRetries, name)
	e.timer.Stop()
	if e.attempting {
		return e.delay
	}
	return 0
}

func (r *Registry) scheduleSpawnRetry(cfg config.Driver, prev time.Duration, cause error) {
	delay := spawnRetryMinDelay
	if prev > 0 {
		delay = min(2*prev, spawnRetryMaxDelay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spawnRetryClosed {
		return
	}
	if r.spawnRetries == nil {
		r.spawnRetries = make(map[string]*spawnRetry)
	}
	e := &spawnRetry{cfg: cfg, delay: delay}
	e.timer = time.AfterFunc(delay, func() { r.retrySpawn(cfg.Name, e) })
	r.spawnRetries[cfg.Name] = e
	slog.Warn("driver failed to start; will retry", "name", cfg.Name, "retry_in", delay, "err", cause)
}

func (r *Registry) retrySpawn(name string, e *spawnRetry) {
	release := r.acquireLifecycle(name)
	defer release()
	r.mu.Lock()
	if r.spawnRetries[name] != e {
		r.mu.Unlock()
		return
	}
	e.attempting = true
	r.mu.Unlock()
	if err := r.add(context.Background(), e.cfg, true); err != nil {
		return
	}
	r.mu.Lock()
	closed := r.spawnRetryClosed
	r.mu.Unlock()
	if closed {
		// Shutdown began while this attempt ran and may have missed it.
		r.removeLocked(name, false)
		return
	}
	slog.Info("driver started on retry", "name", name)
}

func (r *Registry) pendingSpawnRetries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.spawnRetries))
	for n := range r.spawnRetries {
		out = append(out, n)
	}
	return out
}

// closeSpawnRetries stops every pending retry and refuses new ones.
func (r *Registry) closeSpawnRetries() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spawnRetryClosed = true
	for _, e := range r.spawnRetries {
		e.timer.Stop()
	}
	r.spawnRetries = nil
}
