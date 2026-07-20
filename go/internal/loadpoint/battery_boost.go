package loadpoint

import (
	"errors"
	"time"
)

// MaxBatteryBoostDuration is deliberately short: a forgotten permission can
// survive a restart, but can never turn into an open-ended battery-to-EV path.
const MaxBatteryBoostDuration = 4 * time.Hour

const MinBatteryBoostDuration = time.Minute

// BatteryBoostStopReason is stable API/UI vocabulary explaining why core
// withdrew a lease. Empty means no stop has happened in this process.
type BatteryBoostStopReason string

const (
	BatteryBoostStoppedCancelled          BatteryBoostStopReason = "cancelled"
	BatteryBoostStoppedExpired            BatteryBoostStopReason = "expired"
	BatteryBoostStoppedVehicleUnplugged   BatteryBoostStopReason = "vehicle_unplugged"
	BatteryBoostStoppedEVTargetReached    BatteryBoostStopReason = "ev_target_reached"
	BatteryBoostStoppedDepartureReached   BatteryBoostStopReason = "departure_reached"
	BatteryBoostStoppedOperatorHold       BatteryBoostStopReason = "operator_hold"
	BatteryBoostStoppedSurplusOnly        BatteryBoostStopReason = "surplus_only"
	BatteryBoostStoppedSiteSafety         BatteryBoostStopReason = "site_safety_block"
	BatteryBoostStoppedLoadpointDriver    BatteryBoostStopReason = "loadpoint_driver_unavailable"
	BatteryBoostStoppedBatteryUnavailable BatteryBoostStopReason = "battery_unavailable"
	BatteryBoostStoppedBatteryReserve     BatteryBoostStopReason = "battery_reserve_reached"
	BatteryBoostStoppedBatteryHold        BatteryBoostStopReason = "battery_hold"
	BatteryBoostStoppedCoreMode           BatteryBoostStopReason = "core_mode"
	BatteryBoostStoppedFuseSafety         BatteryBoostStopReason = "fuse_safety_block"
	BatteryBoostStoppedRestartInvalid     BatteryBoostStopReason = "restart_lease_invalid"
)

// BatteryBoostLease is the minimal persisted restart envelope. Every lease has
// an absolute expiry; StartedAt plus the duration cap prevents a hand-edited or
// old row from reviving a permission indefinitely after restart.
type BatteryBoostLease struct {
	StartedAt        time.Time `json:"started_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	MinBatterySoCPct float64   `json:"min_battery_soc_pct"`
	EVTargetSoCPct   float64   `json:"ev_target_soc_pct,omitempty"`
	DepartureAt      time.Time `json:"departure_at,omitempty"`
}

// BatteryBoostStatus is returned by the dedicated status endpoint and embedded
// in GET /api/loadpoints. State is one of inactive, active, or stopped.
type BatteryBoostStatus struct {
	State            string                 `json:"state"`
	Active           bool                   `json:"active"`
	StartedAtMs      int64                  `json:"started_at_ms,omitempty"`
	ExpiresAtMs      int64                  `json:"expires_at_ms,omitempty"`
	MinBatterySoCPct float64                `json:"min_battery_soc_pct,omitempty"`
	EVTargetSoCPct   float64                `json:"ev_target_soc_pct,omitempty"`
	DepartureAtMs    int64                  `json:"departure_at_ms,omitempty"`
	StopReason       BatteryBoostStopReason `json:"stop_reason,omitempty"`
	StoppedAtMs      int64                  `json:"stopped_at_ms,omitempty"`
}

// BatteryBoostSafetyFunc is wired by core and returns a stop reason whenever
// current site state cannot safely honour the lease. It deliberately returns
// FTW domain vocabulary instead of exposing telemetry or driver internals.
type BatteryBoostSafetyFunc func(id string, lease BatteryBoostLease) BatteryBoostStopReason

// ValidateBatteryBoostLease validates the durable/wire contract without
// consulting live site state. API callers use it to distinguish malformed
// requests (400) from a currently blocked but well-formed lease (409).
func ValidateBatteryBoostLease(lease BatteryBoostLease, now time.Time) error {
	if lease.StartedAt.IsZero() || lease.ExpiresAt.IsZero() {
		return errors.New("started_at and expires_at are required")
	}
	if lease.StartedAt.After(now.Add(time.Minute)) {
		return errors.New("started_at is in the future")
	}
	if !lease.ExpiresAt.After(now) {
		return errors.New("lease already expired")
	}
	if lease.ExpiresAt.Sub(lease.StartedAt) < MinBatteryBoostDuration {
		return errors.New("lease duration is shorter than 60 seconds")
	}
	if lease.ExpiresAt.Sub(lease.StartedAt) > MaxBatteryBoostDuration {
		return errors.New("lease duration exceeds four hours")
	}
	if lease.ExpiresAt.Sub(now) > MaxBatteryBoostDuration {
		return errors.New("lease remaining duration exceeds four hours")
	}
	if lease.MinBatterySoCPct < 5 || lease.MinBatterySoCPct > 100 {
		return errors.New("min_battery_soc_pct must be 5..100")
	}
	if lease.EVTargetSoCPct < 0 || lease.EVTargetSoCPct > 100 {
		return errors.New("ev_target_soc_pct must be 0..100")
	}
	if !lease.DepartureAt.IsZero() {
		if !lease.DepartureAt.After(now) {
			return errors.New("departure_at must be in the future")
		}
		if lease.DepartureAt.After(lease.ExpiresAt) {
			return errors.New("departure_at must not be after expires_at")
		}
	}
	return nil
}

func batteryBoostStatusFromLease(lease BatteryBoostLease) BatteryBoostStatus {
	status := BatteryBoostStatus{
		State:            "active",
		Active:           true,
		StartedAtMs:      lease.StartedAt.UnixMilli(),
		ExpiresAtMs:      lease.ExpiresAt.UnixMilli(),
		MinBatterySoCPct: lease.MinBatterySoCPct,
		EVTargetSoCPct:   lease.EVTargetSoCPct,
	}
	if !lease.DepartureAt.IsZero() {
		status.DepartureAtMs = lease.DepartureAt.UnixMilli()
	}
	return status
}

// SetBatteryBoostSafety wires the core safety evaluator used both as an API
// preflight and on every dispatch tick.
func (c *Controller) SetBatteryBoostSafety(fn BatteryBoostSafetyFunc) {
	if c == nil {
		return
	}
	c.batteryBoostMu.Lock()
	c.batteryBoostSafety = fn
	c.batteryBoostMu.Unlock()
}

// SetBatteryBoostSaver wires state.db persistence. Restore leases before
// installing the saver so startup reads do not immediately rewrite rows.
func (c *Controller) SetBatteryBoostSaver(fn func(id string, lease BatteryBoostLease, cleared bool)) {
	if c == nil {
		return
	}
	c.batteryBoostMu.Lock()
	c.batteryBoostSaver = fn
	c.batteryBoostMu.Unlock()
}

// SetBatteryBoostStopped wires a non-blocking integration hook (normally an
// MPC replan request) for every active→stopped transition.
func (c *Controller) SetBatteryBoostStopped(fn func(id string, reason BatteryBoostStopReason)) {
	if c == nil {
		return
	}
	c.batteryBoostMu.Lock()
	c.batteryBoostStopped = fn
	c.batteryBoostMu.Unlock()
}

// EnableBatteryBoost installs or replaces a lease after checking current
// loadpoint/operator/core state. The planner is not hardware authority; this
// lease is only permission for the existing validated dispatch path.
func (c *Controller) EnableBatteryBoost(id string, lease BatteryBoostLease, now time.Time) (BatteryBoostStatus, error) {
	if c == nil || c.manager == nil {
		return BatteryBoostStatus{}, errors.New("loadpoint controller not available")
	}
	if err := ValidateBatteryBoostLease(lease, now); err != nil {
		return BatteryBoostStatus{}, err
	}
	if _, err := c.batteryBoostLivePreflight(id, lease, now, false); err != nil {
		return BatteryBoostStatus{}, err
	}

	status := batteryBoostStatusFromLease(lease)
	c.batteryBoostMu.Lock()
	c.batteryBoost[id] = lease
	c.batteryBoostStatus[id] = status
	saver := c.batteryBoostSaver
	c.batteryBoostMu.Unlock()
	if saver != nil {
		saver(id, lease, false)
	}
	return status, nil
}

// batteryBoostLivePreflight centralises the mutable-state checks shared by a
// fresh API enable and restart restoration. Restore requires the core safety
// evaluator to be present: without it, battery/site health cannot be proven
// and a persisted permission must remain stopped.
func (c *Controller) batteryBoostLivePreflight(id string, lease BatteryBoostLease, now time.Time, requireSafety bool) (BatteryBoostStopReason, error) {
	st, ok := c.manager.State(id)
	if !ok {
		return BatteryBoostStoppedRestartInvalid, errors.New("loadpoint not found")
	}
	if !st.PluggedIn {
		return BatteryBoostStoppedVehicleUnplugged, errors.New("vehicle is not plugged in")
	}
	if st.SurplusOnly {
		return BatteryBoostStoppedSurplusOnly, errors.New("surplus_only is an operator clamp")
	}
	if _, held := c.GetManualHold(id, now); held {
		return BatteryBoostStoppedOperatorHold, errors.New("loadpoint operator hold is active")
	}

	c.batteryBoostMu.Lock()
	safety := c.batteryBoostSafety
	c.batteryBoostMu.Unlock()
	if safety == nil && requireSafety {
		return BatteryBoostStoppedRestartInvalid, errors.New("core safety evaluator not available")
	}
	if safety != nil {
		if reason := safety(id, lease); reason != "" {
			return reason, errors.New(string(reason))
		}
	}
	return "", nil
}

// RestoreBatteryBoost restores only a still-valid bounded lease. Invalid or
// expired rows are represented as stopped and must be cleared by the caller.
func (c *Controller) RestoreBatteryBoost(id string, lease BatteryBoostLease, now time.Time) bool {
	if c == nil || c.manager == nil {
		return false
	}
	if err := ValidateBatteryBoostLease(lease, now); err != nil {
		c.recordRejectedBatteryBoostRestore(id, lease, BatteryBoostStoppedRestartInvalid, now)
		return false
	}
	if reason, err := c.batteryBoostLivePreflight(id, lease, now, true); err != nil {
		c.recordRejectedBatteryBoostRestore(id, lease, reason, now)
		return false
	}
	c.batteryBoostMu.Lock()
	c.batteryBoost[id] = lease
	c.batteryBoostStatus[id] = batteryBoostStatusFromLease(lease)
	c.batteryBoostMu.Unlock()
	return true
}

func (c *Controller) recordRejectedBatteryBoostRestore(id string, lease BatteryBoostLease, reason BatteryBoostStopReason, now time.Time) {
	if reason == "" {
		reason = BatteryBoostStoppedRestartInvalid
	}
	status := batteryBoostStatusFromLease(lease)
	if lease.StartedAt.IsZero() {
		status.StartedAtMs = 0
	}
	if lease.ExpiresAt.IsZero() {
		status.ExpiresAtMs = 0
	}
	status.State = "stopped"
	status.Active = false
	status.StopReason = reason
	status.StoppedAtMs = now.UnixMilli()
	c.batteryBoostMu.Lock()
	delete(c.batteryBoost, id)
	c.batteryBoostStatus[id] = status
	c.batteryBoostMu.Unlock()
}

func (c *Controller) stopBatteryBoost(id string, reason BatteryBoostStopReason, now time.Time) BatteryBoostStatus {
	if c == nil {
		return BatteryBoostStatus{State: "inactive"}
	}
	c.batteryBoostMu.Lock()
	lease, existed := c.batteryBoost[id]
	delete(c.batteryBoost, id)
	status := c.batteryBoostStatus[id]
	if existed || status.State == "active" {
		status = batteryBoostStatusFromLease(lease)
		status.State = "stopped"
		status.Active = false
		status.StopReason = reason
		status.StoppedAtMs = now.UnixMilli()
		c.batteryBoostStatus[id] = status
	}
	saver := c.batteryBoostSaver
	stopped := c.batteryBoostStopped
	c.batteryBoostMu.Unlock()
	if saver != nil && existed {
		saver(id, BatteryBoostLease{}, true)
	}
	if stopped != nil && existed {
		stopped(id, reason)
	}
	if !existed && status.State == "" {
		return BatteryBoostStatus{State: "inactive"}
	}
	return status
}

// CancelBatteryBoost is idempotent and retains a user-visible terminal reason.
func (c *Controller) CancelBatteryBoost(id string, now time.Time) BatteryBoostStatus {
	return c.stopBatteryBoost(id, BatteryBoostStoppedCancelled, now)
}

// BatteryBoost returns the active lease and status. Expiry is enforced lazily
// here as well as on dispatch ticks so a status request can never report an
// already-expired permission as active.
func (c *Controller) BatteryBoost(id string, now time.Time) (BatteryBoostLease, BatteryBoostStatus) {
	if c == nil {
		return BatteryBoostLease{}, BatteryBoostStatus{State: "inactive"}
	}
	c.batteryBoostMu.Lock()
	lease, ok := c.batteryBoost[id]
	status := c.batteryBoostStatus[id]
	c.batteryBoostMu.Unlock()
	if ok && !now.Before(lease.ExpiresAt) {
		return BatteryBoostLease{}, c.stopBatteryBoost(id, BatteryBoostStoppedExpired, now)
	}
	if ok {
		return lease, batteryBoostStatusFromLease(lease)
	}
	if status.State == "" {
		status.State = "inactive"
	}
	return BatteryBoostLease{}, status
}

func (c *Controller) evaluateBatteryBoost(id string, now time.Time, connected, dispatchAllowed bool) {
	lease, status := c.BatteryBoost(id, now)
	if !status.Active {
		return
	}
	if !connected {
		c.stopBatteryBoost(id, BatteryBoostStoppedVehicleUnplugged, now)
		return
	}
	if !dispatchAllowed {
		c.stopBatteryBoost(id, BatteryBoostStoppedSiteSafety, now)
		return
	}
	if _, held := c.GetManualHold(id, now); held {
		c.stopBatteryBoost(id, BatteryBoostStoppedOperatorHold, now)
		return
	}
	if st, ok := c.manager.State(id); ok {
		if st.SurplusOnly {
			c.stopBatteryBoost(id, BatteryBoostStoppedSurplusOnly, now)
			return
		}
		if lease.EVTargetSoCPct > 0 && st.CurrentSoCPct >= lease.EVTargetSoCPct {
			c.stopBatteryBoost(id, BatteryBoostStoppedEVTargetReached, now)
			return
		}
	}
	if !lease.DepartureAt.IsZero() && !now.Before(lease.DepartureAt) {
		c.stopBatteryBoost(id, BatteryBoostStoppedDepartureReached, now)
		return
	}
	c.batteryBoostMu.Lock()
	safety := c.batteryBoostSafety
	c.batteryBoostMu.Unlock()
	if safety != nil {
		if reason := safety(id, lease); reason != "" {
			c.stopBatteryBoost(id, reason, now)
		}
	}
}

// ActiveBatteryBoostTotals returns the authorised live EV watts and the most
// conservative reserve among active leases. The caller supplies one coherent
// loadpoint snapshot from the current tick.
func (c *Controller) ActiveBatteryBoostTotals(states []State, now time.Time) (powerW, reserveSoC float64) {
	for _, st := range states {
		lease, status := c.BatteryBoost(st.ID, now)
		if !status.Active || !st.PluggedIn || st.SurplusOnly {
			continue
		}
		if st.CurrentPowerW > 0 {
			powerW += st.CurrentPowerW
		}
		if lease.MinBatterySoCPct/100 > reserveSoC {
			reserveSoC = lease.MinBatterySoCPct / 100
		}
	}
	return powerW, reserveSoC
}
