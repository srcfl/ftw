package ocpp

// A charger FTW steers starts at 0 A.
//
// Until a charging profile lands, a charger runs at its own default, which is
// usually its rating. FTW orders a plugged-in car every control tick, but the
// first order still takes a tick plus the charger's own response time. A car
// plugged in during that gap draws the full rating on a fuse FTW has not yet
// shared out. So on each BootNotification from a charger a loadpoint names,
// FTW sends the same profile it sends to pause: TxDefaultProfile, id 1, stack
// level 0, 0 A for the whole charger. Its first real order has the same id,
// purpose and stack level, so it replaces this one (OCPP 1.6 §3.13.2, 2.0.1
// K01.FR.05).

import (
	"context"
	"log/slog"
)

// SetManagedIDs declares which approved chargers FTW orders: those a loadpoint
// names. An id listed only under ocpp.chargers is approved and metered but
// never ordered, so a 0 A start would hold it at 0 A for good. Replaces the
// previous set; a change applies at the charger's next boot.
func (h *Handler) SetManagedIDs(ids []string) {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			m[id] = true
		}
	}
	h.mu.Lock()
	h.managed = m
	h.mu.Unlock()
}

// maybeSendBootLimit sends the 0 A start limit after a BootNotification.
//
// It skips a boot that answers FTW's own identity probe: that charger did not
// restart, and may be charging under FTW's last order. It also skips a boot
// when the last status FTW holds shows a car or an open transaction, on this
// socket or the one before: FTW orders that car every tick. Much firmware
// sends its own BootNotification on every reconnect, before any status, so
// after a Wi-Fi drop the new socket alone says nothing about the car.
// The send runs on its own goroutine, as the capability probe does, and goes
// through the same one-at-a-time slot as every other order. A refusal is
// logged once; the next boot tries again.
func (h *Handler) maybeSendBootLimit(id string) {
	h.mu.Lock()
	s := h.chargersLocked(id)
	send := h.bootLimit
	if !h.approved[id] || !h.managed[id] || s.identityProbeAttempts > 0 || s.connected || s.transactionID >= 0 {
		send = nil
	}
	h.mu.Unlock()
	if send != nil {
		go send(id)
	}
}

// sendBootLimit is the Server side of maybeSendBootLimit.
func (s *Server) sendBootLimit(id string) {
	if err := s.setLimit(context.Background(), id, 0, nil); err != nil {
		slog.Warn("ocpp: charger did not take the 0 A start limit; it runs at its own default until FTW's first order",
			"charger", id, "err", err)
	}
}
