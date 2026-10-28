package ocpp

// A charger FTW steers starts at 0 A.
//
// Until a charging profile lands, a charger runs at its own default, which is
// usually its rating. FTW orders a plugged-in car every control tick, but the
// first order still takes a tick plus the charger's own response time. A car
// plugged in during that gap draws the full rating on a fuse FTW has not yet
// shared out. So when a charger a loadpoint names boots and then reports no
// car, FTW sends the same profile it sends to pause: TxDefaultProfile, id 1,
// stack level 0, 0 A for the whole charger. Its first real order has the same
// id, purpose and stack level, so it replaces this one (OCPP 1.6 §3.13.2,
// 2.0.1 K01.FR.05).

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

// maybeSendBootLimit marks a BootNotification for the 0 A start limit. The
// limit goes out on the first connector status after it, and only when that
// status shows no car (resolveBootLimit).
//
// A boot alone does not show a fresh start. After a Core restart the new
// process holds no status and no transaction, and much firmware sends its own
// BootNotification on reconnect, before FTW's identity probe, while the car
// goes on charging. A 1.6 charger reports every connector's status after an
// accepted boot (§4.9), and 2.0.1 stations send StatusNotification there too,
// so FTW waits for it. A charger that never reports one gets no start limit,
// as before this file existed.
//
// It skips a boot that answers FTW's own identity probe: that charger did not
// restart, and may be charging under FTW's last order. It also skips a boot
// when the last status FTW holds shows a car or an open transaction, on this
// socket or the one before: FTW orders that car every tick.
func (h *Handler) maybeSendBootLimit(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.chargersLocked(id)
	s.bootLimitPending = h.bootLimit != nil && h.approved[id] && h.managed[id] &&
		s.identityProbeAttempts == 0 && !s.connected && s.transactionID < 0
}

// resolveBootLimit runs after each connector status. The first one after a
// marked boot decides: no car and no transaction sends the start limit; a car
// sends nothing, since FTW orders that car every tick. The send runs on its
// own goroutine, as the capability probe does, and goes through the same
// one-at-a-time slot as every other order. A refusal is logged once; the next
// boot tries again.
func (h *Handler) resolveBootLimit(id string) {
	h.mu.Lock()
	s := h.chargersLocked(id)
	send := h.bootLimit
	if !s.bootLimitPending || !h.approved[id] || !h.managed[id] || s.connected || s.transactionID >= 0 {
		send = nil
	}
	s.bootLimitPending = false
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
