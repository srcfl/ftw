package ocpp

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
)

type identityProbeTiming struct{ grace, timeout, interval time.Duration }

var defaultIdentityProbeTiming = identityProbeTiming{time.Second, 10 * time.Second, time.Minute}

const maxIdentityProbeAttempts = 3

var errIdentityProbeUnsupported = errors.New("BootNotification trigger is not supported")

// One timer per charger covers the initial grace, response deadline and retry
// delay. The rate limit survives reconnects. Neither Accepted nor a historical
// serial establishes identity; only a fresh BootNotification does that.
func (h *Handler) scheduleIdentityProbeLocked(id string, s *chargerState) {
	if h.identityProbeStopped || h.identityProbe == nil || !h.approved[id] || !s.online ||
		s.identityCurrent || s.identityProbeTimer != nil || s.identityProbeAttempts >= maxIdentityProbeAttempts {
		return
	}
	delay := h.identityProbeTiming.grace
	if remaining := time.Until(s.identityProbeAfter); remaining > delay {
		delay = remaining
	}
	s.identityProbeEpoch++
	epoch := s.identityProbeEpoch
	s.identityProbeTimer = time.AfterFunc(delay, func() { h.requestIdentity(id, epoch) })
}

func (h *Handler) cancelIdentityProbeLocked(s *chargerState) {
	s.identityProbeEpoch++
	if s.identityProbeTimer != nil {
		s.identityProbeTimer.Stop()
		s.identityProbeTimer = nil
	}
}

func (h *Handler) requestIdentity(id string, epoch uint64) {
	h.mu.Lock()
	s := h.chargers[id]
	if s == nil || s.identityProbeEpoch != epoch {
		h.mu.Unlock()
		return
	}
	s.identityProbeTimer = nil
	if h.identityProbeStopped || !h.approved[id] || !s.online || s.identityCurrent {
		h.mu.Unlock()
		return
	}
	s.identityProbeAttempts++
	s.identityProbeAfter = time.Now().Add(h.identityProbeTiming.interval)
	s.identityProbeTimer = time.AfterFunc(h.identityProbeTiming.timeout, func() {
		h.identityProbeFailed(id, epoch, errors.New("fresh BootNotification did not arrive before the deadline"))
	})
	probe, version := h.identityProbe, s.version
	h.mu.Unlock()
	if err := probe(id, version, func(err error) {
		if err != nil {
			h.identityProbeFailed(id, epoch, err)
		}
	}); err != nil {
		h.identityProbeFailed(id, epoch, err)
	}
}

func (h *Handler) identityProbeFailed(id string, epoch uint64, err error) {
	h.mu.Lock()
	s := h.chargers[id]
	if s == nil || s.identityProbeEpoch != epoch {
		h.mu.Unlock()
		return
	}
	h.cancelIdentityProbeLocked(s)
	if errors.Is(err, errIdentityProbeUnsupported) {
		s.identityProbeAttempts = maxIdentityProbeAttempts
	}
	h.scheduleIdentityProbeLocked(id, s)
	h.mu.Unlock()
	slog.Info("ocpp: charger identity remains unconfirmed", "charger", id, "err", err)
}

func (h *Handler) stopIdentityProbes() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.identityProbeStopped = true
	for _, s := range h.chargers {
		h.cancelIdentityProbeLocked(s)
	}
}

func (s *Server) requestBootNotification(id string, version Version, done func(error)) error {
	alias, err := s.sockets.currentID(id)
	if err != nil {
		return err
	}
	if version == Version201 && s.csms != nil {
		return s.csms.TriggerMessage(alias, func(reply *remotecontrol.TriggerMessageResponse, err error) {
			if err != nil {
				done(err)
				return
			}
			if reply != nil && reply.Status == remotecontrol.TriggerMessageStatusAccepted {
				return
			}
			if reply != nil && reply.Status == remotecontrol.TriggerMessageStatusNotImplemented {
				done(errIdentityProbeUnsupported)
				return
			}
			done(fmt.Errorf("BootNotification trigger rejected: %v", reply))
		}, remotecontrol.MessageTriggerBootNotification)
	}
	return s.cs.TriggerMessage(alias, func(reply *remotetrigger.TriggerMessageConfirmation, err error) {
		if err != nil {
			done(err)
			return
		}
		if reply != nil && reply.Status == remotetrigger.TriggerMessageStatusAccepted {
			return
		}
		if reply != nil && reply.Status == remotetrigger.TriggerMessageStatusNotImplemented {
			done(errIdentityProbeUnsupported)
			return
		}
		done(fmt.Errorf("BootNotification trigger rejected: %v", reply))
	}, remotetrigger.MessageTrigger(core.BootNotificationFeatureName))
}
