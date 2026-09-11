package loadpoint

import (
	"math"
	"time"
)

// ManualStatus is the live account of an operator hold ("Charge now"):
// what was asked, what the box ordered after its clamps, since when, and
// what the charger did with it. The manual tab reads it every poll, so the
// operator never has to guess from a 0 W readout whether the button worked.
// Field report 2026-09-05 (#1002): Charge now at 22:00, the tab said
// "Charging at 16 A" within a tenth of a second, the Easee cloud takes
// 5–15 s to act, nothing on screen moved, and the operator removed the
// charger to charge by hand.
type ManualStatus struct {
	Active             bool  `json:"active"`
	ChargerUpdatedAtMs int64 `json:"charger_updated_at_ms,omitempty"`
	// State is one of ManualSent, ManualAccepted, ManualCharging,
	// ManualNotDrawing, ManualStalled or ManualLimited. Empty when inactive.
	State string `json:"state,omitempty"`
	// StartedAtMs is when the operator installed the hold.
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
	// SinceMs is when the current order took effect: the hold's start, or
	// the last change of the ordered watts (an Update on the amp slider, a
	// fuse clamp coming or going). Elapsed time in the UI counts from here.
	SinceMs int64 `json:"since_ms,omitempty"`
	// RequestedW is what the operator asked for; CommandedW is what the box
	// ordered after every clamp. They differ while the main fuse limits.
	RequestedW float64 `json:"requested_w"`
	CommandedW float64 `json:"commanded_w,omitempty"`
	RequestedA float64 `json:"requested_a"`
	CommandedA float64 `json:"commanded_a,omitempty"`
	// ChargerLimitA is the current limit the charger itself reports, when
	// its driver exposes one (Easee: max_a). ChargerLimitKnown separates a
	// reading of zero from no reading at all.
	ChargerLimitA     float64 `json:"charger_limit_a,omitempty"`
	ChargerLimitKnown bool    `json:"charger_limit_known,omitempty"`
	// ChargerReason is the charger's own explanation for delivering no
	// current, in its words (reason_no_current_label). Empty when it has none.
	ChargerReason string `json:"charger_reason,omitempty"`
	// LimitReason names the clamp behind ManualLimited: "fuse_limit",
	// "fuse_cooldown" or "site_meter_stale".
	LimitReason string `json:"limit_reason,omitempty"`
}

const (
	// ManualSent: the hold is installed; the charger has not yet reflected
	// the ordered limit.
	ManualSent = "sent"
	// ManualAccepted: the charger reports the ordered limit; the car has not
	// started drawing yet.
	ManualAccepted = "accepted"
	// ManualCharging: power is flowing.
	ManualCharging = "charging"
	// ManualNotDrawing: the charger reports the ordered limit, and the car
	// still draws nothing after the grace period.
	ManualNotDrawing = "not_drawing"
	// ManualStalled: the charger says the command stalled, or never
	// confirmed it within the timeout.
	ManualStalled = "stalled"
	// ManualLimited: a clamp the hold cannot override (main fuse, stale site
	// meter) holds the order below what was asked.
	ManualLimited     = "limited"
	ManualUnavailable = "unavailable"
	ManualPausing     = "pausing"
	ManualPaused      = "paused"
)

// ChargerStatus separates a current report from a cached reading.
// Power and connection state cannot be treated as current when Available is false.
type ChargerStatus struct {
	Known       bool     `json:"known"`
	Available   bool     `json:"available"`
	UpdatedAtMs int64    `json:"updated_at_ms,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	LimitA      *float64 `json:"limit_a,omitempty"`
}

// ChargerReading is what the charger's driver last reported, as far as the
// manual status needs it. Known is false when there is no reading.
type ChargerReading struct {
	Known       bool
	UpdatedAt   time.Time
	Unavailable bool
	LimitA      float64
	LimitKnown  bool
	Charging    bool
	Reason      string
	Stalled     bool
}

const (
	// manualAcceptGrace is how long a charger that has taken the limit may
	// sit at 0 W before the box calls it "not drawing". An Easee contactor
	// plus the car's ramp takes 5–15 s; a phase flip up to 90 s.
	manualAcceptGrace = 2 * time.Minute
	// manualConfirmTimeout is how long the box waits for the charger to
	// reflect the ordered limit at all before calling the command stalled.
	manualConfirmTimeout = 3 * time.Minute
	// manualChargingFloorW is the draw above which the hold counts as
	// charging. Same floor the plan strip uses.
	manualChargingFloorW = 100.0
)

// holdClampReason reports whether a commanded reason is one of the clamps
// that override an operator hold. Any other reason belongs to a tick from
// before the hold was installed and says nothing about it.
func holdClampReason(reason string) bool {
	switch reason {
	case "fuse_limit", "fuse_cooldown", "site_meter_stale", "charger_limit":
		return true
	}
	return false
}

// ManualStatusFrom derives the operator-facing status of a hold from the
// controller's hold, the loadpoint snapshot (with Phases and VoltageV set)
// and the charger's reading. Pure, so a tick-by-tick test can drive it; the
// API layer calls it on every poll.
func ManualStatusFrom(h ManualHold, held bool, st State, ch ChargerReading, now time.Time) ManualStatus {
	if !held {
		return ManualStatus{}
	}
	perA := float64(st.Phases) * st.VoltageV
	toA := func(w float64) float64 {
		if perA <= 0 || w <= 0 {
			return 0
		}
		return math.Round(w/perA*10) / 10
	}
	m := ManualStatus{
		Active:            true,
		RequestedW:        h.PowerW,
		RequestedA:        toA(h.PowerW),
		ChargerLimitA:     ch.LimitA,
		ChargerLimitKnown: ch.Known && ch.LimitKnown,
		ChargerReason:     ch.Reason,
	}
	if !ch.UpdatedAt.IsZero() {
		m.ChargerUpdatedAtMs = ch.UpdatedAt.UnixMilli()
	}
	// The ordered value is the box's last command only once a tick has run
	// the hold branch; before that the snapshot still carries the previous
	// automatic order, which says nothing about this hold.
	clamp := st.CommandedKnown && holdClampReason(st.CommandedReason)
	ordered := h.PowerW
	if st.CommandedKnown && (st.CommandedReason == "manual_hold" || clamp) {
		ordered = st.CommandedW
	}
	m.CommandedW = ordered
	m.CommandedA = toA(ordered)

	since := h.StartedAt
	if h.UpdatedAt.After(since) {
		since = h.UpdatedAt
	}
	if !h.StartedAt.IsZero() {
		m.StartedAtMs = h.StartedAt.UnixMilli()
	}
	if st.CommandedSinceMs > 0 && (st.CommandedReason == "manual_hold" || clamp) {
		if t := time.UnixMilli(st.CommandedSinceMs); t.After(since) {
			since = t
		}
	}
	var elapsed time.Duration
	if !since.IsZero() {
		m.SinceMs = since.UnixMilli()
		elapsed = now.Sub(since)
	}

	commandMatches := h.UpdatedAt.IsZero() || st.ManualCommandUpdatedAt.Equal(h.UpdatedAt)
	if h.PowerW == 0 {
		m.State = ManualPausing
		switch {
		case ch.Unavailable:
			m.State = ManualUnavailable
		case commandMatches && st.CommandedKnown && st.CommandedReason == "manual_hold" && st.CommandedW == 0 &&
			!ch.UpdatedAt.IsZero() && !ch.UpdatedAt.Before(since) &&
			ch.Known && !ch.Charging && st.CurrentPowerW < manualChargingFloorW &&
			(!ch.LimitKnown || ch.LimitA < 0.1):
			m.State = ManualPaused
		case elapsed >= manualConfirmTimeout:
			m.State = ManualStalled
		}
		return m
	}

	limitMatches := m.ChargerLimitKnown && m.CommandedA >= 0 && math.Abs(ch.LimitA-m.CommandedA) < 1
	switch {
	case ch.Unavailable:
		m.State = ManualUnavailable
	case !commandMatches && elapsed >= manualConfirmTimeout:
		m.State = ManualStalled
	case !commandMatches:
		m.State = ManualSent
	case ch.Known && ch.Stalled:
		m.State = ManualStalled
	case m.ChargerLimitKnown && !limitMatches && elapsed >= manualConfirmTimeout:
		m.State = ManualStalled
	case (m.ChargerLimitKnown && !limitMatches) || (!ch.UpdatedAt.IsZero() && ch.UpdatedAt.Before(since)):
		m.State = ManualSent
	case st.CurrentPowerW >= manualChargingFloorW || (ch.Known && ch.Charging):
		m.State = ManualCharging
		if clamp {
			m.LimitReason = st.CommandedReason
		}
	case clamp:
		m.State = ManualLimited
		m.LimitReason = st.CommandedReason
	case limitMatches && elapsed >= manualAcceptGrace:
		m.State = ManualNotDrawing
	case limitMatches:
		m.State = ManualAccepted
	case elapsed >= manualConfirmTimeout:
		m.State = ManualStalled
	default:
		m.State = ManualSent
	}
	return m
}
