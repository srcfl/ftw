package loadpoint

import (
	"context"
	"log/slog"
	"time"
)

type resumeOffer struct {
	driver, device, session string
	generation              uint64
	nextAttempt             time.Time
	retryDelay              time.Duration
}

// resumeAfterZeroOffer remembers our standdown independently of commanded W.
// Resume is optional: ev_set_current still runs and alone decides driver health.
// Retries use the next normal tick, the current safety gates and a bounded rate.
func (c *Controller) resumeAfterZeroOffer(ctx context.Context, cfg Config, sample EVSample, offerW float64, now time.Time) {
	if c.resumeOffers == nil {
		c.resumeOffers = make(map[string]resumeOffer)
	}
	proof := resumeOffer{driver: cfg.DriverName, device: sample.DeviceID, session: sample.SessionID, generation: sample.ConnectionGeneration}
	previous, exists := c.resumeOffers[cfg.ID]
	if previous.driver != proof.driver || previous.device != proof.device || previous.session != proof.session || previous.generation != proof.generation {
		delete(c.resumeOffers, cfg.ID)
		exists = false
	}
	if offerW <= 0 {
		c.resumeOffers[cfg.ID] = proof
		return
	}
	if sample.PowerW >= DeliveringW {
		delete(c.resumeOffers, cfg.ID)
		return
	}
	if !exists || now.Before(previous.nextAttempt) {
		return
	}
	if err := c.sendDispatchWithDeadline(ctx, cfg.DriverName, []byte(`{"action":"ev_resume"}`)); err != nil {
		previous.retryDelay = min(time.Minute, max(5*time.Second, 2*previous.retryDelay))
		previous.nextAttempt = now.Add(previous.retryDelay)
		c.resumeOffers[cfg.ID] = previous
		slog.Warn("loadpoint resume after zero offer failed", "lp", cfg.ID, "driver", cfg.DriverName,
			"retry_at", previous.nextAttempt, "err", err)
		return
	}
	delete(c.resumeOffers, cfg.ID)
}
