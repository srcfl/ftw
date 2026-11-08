package loadpoint

import (
	"context"
	"log/slog"
	"math"
	"time"
)

type resumeOffer struct {
	driver, device, session   string
	generation                uint64
	nextAttempt               time.Time
	retryDelay                time.Duration
	acknowledgedAttempts      int
	deliverySince, deliveryAt time.Time
	zeroOffer                 bool
	noDrawSince, noDrawAt     time.Time
}

// Allow a missed five-second tick and ordinary cloud poll jitter. Repeating
// one timestamp still cannot advance either delivery or no-draw proof.
const resumeObservationGap = 15 * time.Second

// Commands may block while Stop or a new plug session arrives. Use this
// check after either charger dispatch or a vehicle start attempt.
func (c *Controller) currentChargeSample(now time.Time, cfg Config, previous EVSample) (EVSample, time.Time, bool) {
	if hold, held := c.GetManualHold(cfg.ID, now); held && hold.PowerW == 0 {
		return EVSample{}, now, false
	}
	if c.tel == nil {
		return previous, now, true
	}
	sample, live := c.tel(cfg.DriverName)
	if !live || !sample.Connected || sample.ConnectionUnknown || sample.DeviceID != previous.DeviceID ||
		sample.SessionID != previous.SessionID || sample.ConnectionGeneration != previous.ConnectionGeneration {
		return sample, now, false
	}
	if checkedAt := time.Now(); sample.PowerAt.After(now) && !sample.PowerAt.After(checkedAt) {
		now = checkedAt
	}
	return sample, now, true
}

func chargerIdleForWake(sample EVSample, now time.Time) bool {
	return !sample.PowerUnavailable && finite(sample.PowerW) && sample.PowerW < DeliveringW &&
		!sample.PowerAt.After(now) && (sample.PowerAt.IsZero() || now.Sub(sample.PowerAt) <= sample.PowerWindow())
}

// vehicleDeclinesOffer needs fresh charger data that says the car stopped
// asking, and a stop that has lasted SessionCompletionTimeout while Core
// offered current. A handshake blip, Core's own pause, or stale or unknown
// evidence is not enough. It says nothing about the battery level.
func (c *Controller) vehicleDeclinesOffer(id string, sample EVSample, now time.Time) bool {
	if c == nil || c.manager == nil || !sample.RequestKnown || sample.RequestActive || !sample.Connected || sample.ConnectionUnknown ||
		sample.PowerUnavailable || !finite(sample.PowerW) || sample.PowerW < 0 || sample.PowerW >= DeliveringW ||
		sample.PowerAt.IsZero() || sample.PowerAt.After(now) || now.Sub(sample.PowerAt) > min(sample.PowerWindow(), resumeObservationGap) {
		return false
	}
	st, ok := c.manager.State(id)
	return ok && st.VehicleDeclinesOffer(now)
}

// recoverOfferedCharge resumes our zero offer, or a known plug session that
// still asks for charge after two minutes of accepted positive offers without
// delivery. The current limit must succeed before this optional resume. Only
// measured delivery ends recovery; an acknowledgement is not a charge.
func (c *Controller) recoverOfferedCharge(ctx context.Context, cfg Config, sample EVSample, offerW float64, now time.Time) {
	if c.resumeOffers == nil {
		c.resumeOffers = make(map[string]*resumeOffer)
	}
	proof := resumeOffer{driver: cfg.DriverName, device: sample.DeviceID, session: sample.SessionID, generation: sample.ConnectionGeneration}
	previous := c.resumeOffers[cfg.ID]
	if previous != nil && (previous.driver != proof.driver || previous.device != proof.device || previous.session != proof.session || previous.generation != proof.generation) {
		delete(c.resumeOffers, cfg.ID)
		previous = nil
	}
	if offerW <= 0 {
		proof.zeroOffer = true
		c.resumeOffers[cfg.ID] = &proof
		return
	}
	// A lasting vehicle-side stop is not a failed start. Do not infer a full
	// battery or mark its goal complete; a later active request re-enables
	// recovery. A brief stop still gets the resume after Core's own pause.
	if c.vehicleDeclinesOffer(cfg.ID, sample, now) {
		delete(c.resumeOffers, cfg.ID)
		return
	}
	// Ten seconds near the supported minimum prove recovery. A normal
	// charge with no recovery in progress needs no retry record at all.
	minimum := cfg.MinChargeW
	if minimum <= 0 {
		minimum = offerW
	}
	floor := math.Max(DeliveringW, 0.9*math.Min(minimum, offerW))
	if previous == nil {
		if proof.device == "" || proof.session == "" || !sample.RequestActive || sample.PowerW >= floor {
			return
		}
		previous = &proof
		previous.noDrawSince = now
		c.resumeOffers[cfg.ID] = previous
	}
	if !previous.zeroOffer && !sample.RequestActive {
		delete(c.resumeOffers, cfg.ID)
		return
	}
	observedAt := sample.PowerAt
	if observedAt.IsZero() {
		observedAt = now // Legacy telemetry uses the fresh-read contract.
	}
	fresh := !sample.PowerUnavailable && !observedAt.After(now) && now.Sub(observedAt) <= min(sample.PowerWindow(), resumeObservationGap)
	if !fresh || !finite(sample.PowerW) {
		previous.deliverySince, previous.deliveryAt = time.Time{}, time.Time{}
		previous.noDrawSince, previous.noDrawAt = time.Time{}, time.Time{}
		return
	}
	if sample.PowerW >= floor {
		if previous.deliverySince.IsZero() || observedAt.Before(previous.deliveryAt) || observedAt.Sub(previous.deliveryAt) > resumeObservationGap {
			previous.deliverySince = observedAt
		}
		previous.deliveryAt = observedAt
		if observedAt.Sub(previous.deliverySince) >= 10*time.Second {
			delete(c.resumeOffers, cfg.ID)
		}
		return // Already drawing: observe it rather than sending another resume.
	}
	previous.deliverySince, previous.deliveryAt = time.Time{}, time.Time{}
	if !previous.zeroOffer {
		if previous.noDrawSince.IsZero() || observedAt.Before(previous.noDrawAt) ||
			(!previous.noDrawAt.IsZero() && observedAt.Sub(previous.noDrawAt) > resumeObservationGap) {
			previous.noDrawSince = now
		}
		previous.noDrawAt = observedAt
		if previous.noDrawAt.Sub(previous.noDrawSince) < 2*time.Minute {
			return
		}
	}
	if now.Before(previous.nextAttempt) {
		return
	}
	if previous.acknowledgedAttempts >= 3 {
		if previous.acknowledgedAttempts == 3 {
			slog.Warn("loadpoint resume acknowledged but no charge observed", "lp", cfg.ID, "driver", cfg.DriverName)
			previous.acknowledgedAttempts++
		}
		return
	}
	if err := c.sendDispatchWithDeadline(ctx, cfg.DriverName, []byte(`{"action":"ev_resume"}`)); err != nil {
		previous.retryDelay = min(time.Minute, max(5*time.Second, 2*previous.retryDelay))
		previous.nextAttempt = now.Add(previous.retryDelay)
		slog.Warn("loadpoint resume failed", "lp", cfg.ID, "driver", cfg.DriverName,
			"retry_at", previous.nextAttempt, "err", err)
		return
	}
	// An HTTP acknowledgement does not establish that the contactor closed.
	// Retry at a bounded rate, under the current dispatch gates, and stop after
	// three acknowledged attempts. Only observed delivery clears the proof.
	previous.acknowledgedAttempts++
	slog.Info("loadpoint resume acknowledged; waiting for measured charge", "lp", cfg.ID, "driver", cfg.DriverName,
		"attempt", previous.acknowledgedAttempts, "offer_w", offerW, "observed_w", sample.PowerW, "power_at", sample.PowerAt)
	previous.nextAttempt = now.Add(30 * time.Second)
	previous.retryDelay = 0
}
