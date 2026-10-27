package loadpoint

import "time"

type manualSessionBinding struct {
	deviceID, sessionID  string
	generation           uint64
	configuration        uint64
	connectionGeneration uint64
	// proofLost keeps the last proven hardware while the charger cannot
	// prove it, so the next proof is compared with it, not with nothing.
	proofLost bool
}

func otherSession(previous, current manualSessionBinding) bool {
	return previous.sessionID != "" && current.sessionID != "" && previous.sessionID != current.sessionID
}

func (m *Manager) manualSessionBinding(id string) manualSessionBinding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if lp := m.byID[id]; lp != nil {
		return manualSessionBinding{deviceID: lp.sessionDeviceID, sessionID: lp.sessionID, generation: lp.sessionGeneration,
			configuration: lp.configGeneration, connectionGeneration: lp.connectionGeneration}
	}
	return manualSessionBinding{}
}

// markManualExplicit prevents a delayed first telemetry sample from replacing
// an action the user already took. Call outside holdMu in Set/ClearManualHold.
func (c *Controller) markManualExplicit(id string) bool {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.manualRestored == nil {
		c.manualRestored = map[string]bool{}
	}
	first := !c.manualRestored[id]
	c.manualRestored[id] = true
	if c.manager != nil {
		if c.manualBindings == nil {
			c.manualBindings = map[string]manualSessionBinding{}
		}
		c.manualBindings[id] = c.manager.manualSessionBinding(id)
		c.manager.SetManualRestoreUnconfirmed(id, false)
	}
	return first
}

// restoreManualHoldForSession runs after ObserveSession, before dispatch. The
// disk read stays outside holdMu; recheck explicit actions before installing.
func (c *Controller) restoreManualHoldForSession(id string) {
	if c.manager == nil {
		return
	}
	c.manualPersistMu.Lock()
	defer c.manualPersistMu.Unlock()
	current := c.manager.manualSessionBinding(id)
	c.holdMu.Lock()
	if c.manualBindings == nil {
		c.manualBindings = map[string]manualSessionBinding{}
	}
	previous, bound := c.manualBindings[id]
	changed := bound && previous.configuration != current.configuration
	// A dropped socket or a lost hardware proof is not an unplug. Keep the
	// last proof until the charger proves itself again: the same charger
	// keeps Charge now, other hardware or another session does not.
	reconnected := bound && (previous.proofLost || (previous.connectionGeneration != 0 &&
		current.connectionGeneration != 0 && previous.connectionGeneration != current.connectionGeneration))
	if bound && previous.deviceID != "" {
		switch {
		case current.deviceID == "":
			if !changed {
				current = previous
				current.proofLost = true
			}
		case reconnected:
			changed = changed || previous.deviceID != current.deviceID || otherSession(previous, current)
			if !changed && current.sessionID == "" {
				// A driver can report the session after the hardware. Check
				// the next session against the last proven one.
				current.sessionID = previous.sessionID
			}
		default:
			changed = changed || previous.deviceID != current.deviceID || otherSession(previous, current)
			// ObserveSession preserves the generation for a valid first proof.
			// A changed generation therefore means a counter reset or a new
			// session, even when the earlier session ID was still unknown.
			changed = changed || previous.generation != current.generation
		}
	}
	// A Start before the first hardware reading binds here. Once bound, a
	// different hardware/session/config generation must not inherit its power.
	// A restore still waiting for proof has no power to inherit; the saved
	// record is checked against the next identity instead of overwritten.
	c.manualBindings[id] = current
	restored, seen := c.manualRestored[id]
	if pending := seen && !restored; changed && !pending {
		if hold, held := c.holds[id]; held {
			if hold.PowerW > 0 {
				c.holds[id] = ManualHold{Persistent: true}
				c.manager.SetManualRestoreUnconfirmed(id, true)
			}
			c.manualRestored[id] = true
			c.holdMu.Unlock()
			// Keep the safe pause across another restart. The next explicit
			// Start or Return to plan decides how this new session proceeds.
			_ = c.manager.PersistManualHold(id, ManualHold{Persistent: true}, false)
			c.resetManualIdle(id)
			return
		}
	}
	done := c.manualRestored[id]
	c.holdMu.Unlock()
	if done {
		return
	}
	h, status := c.manager.RestoreManualHold(id)
	if status == "pending" && !h.Persistent {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.manualRestored[id] {
		return
	}
	if c.manualRestored == nil {
		c.manualRestored = map[string]bool{}
	}
	_, hadPending := c.manualRestored[id]
	c.manualRestored[id] = status != "pending"
	if status == "pending" || status == "restored" || status == "unconfirmed" {
		if c.holds == nil {
			c.holds = map[string]ManualHold{}
		}
		c.holds[id] = h
		c.manager.SetManualRestoreUnconfirmed(id, status == "unconfirmed" || status == "pending")
	} else if hadPending {
		delete(c.holds, id)
		c.manager.SetManualRestoreUnconfirmed(id, false)
	}
}

// UnplugGrace is how long a charger must say the cable is out before the
// session ends and Charge now is cleared. The cars runtime uses it too.
const UnplugGrace = 30 * time.Second

// holdInUnplugGrace times the unplug and reports whether a hold waits out
// UnplugGrace. An unplugged reading says nothing about the next car, so the
// caller skips the session check. A cable back within the grace keeps the
// hold for the same car; another device or session clears it.
func (c *Controller) holdInUnplugGrace(id string, connected bool, now time.Time) bool {
	if connected {
		if _, pending := c.unpluggedSince[id]; pending {
			delete(c.unpluggedSince, id)
			c.rebindAfterUnplug(id)
		}
		return false
	}
	if c.unpluggedSince == nil {
		c.unpluggedSince = map[string]time.Time{}
	}
	since, seen := c.unpluggedSince[id]
	if !seen {
		since = now
		c.unpluggedSince[id] = now
	}
	c.holdMu.Lock()
	_, held := c.holds[id]
	c.holdMu.Unlock()
	return held && now.Sub(since) < UnplugGrace
}

// rebindAfterUnplug compares the first reading after a short unplug with
// the hardware the hold was bound to before it.
func (c *Controller) rebindAfterUnplug(id string) {
	current := c.manager.manualSessionBinding(id)
	c.holdMu.Lock()
	previous, bound := c.manualBindings[id]
	_, held := c.holds[id]
	other := bound && ((previous.deviceID != "" && current.deviceID != "" && previous.deviceID != current.deviceID) ||
		otherSession(previous, current))
	if held && bound && !other && current.deviceID != "" {
		// The unplug may have moved the session generation. The same
		// hardware and session keep the hold; a later session proof is
		// checked against the one before the unplug.
		if current.sessionID == "" {
			current.sessionID = previous.sessionID
		}
		c.manualBindings[id] = current
	}
	c.holdMu.Unlock()
	if held && other {
		c.ClearManualHold(id)
	}
}
