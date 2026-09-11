package loadpoint

import (
	"encoding/json"
	"fmt"
	"strings"
)

type storedManualHold struct {
	Version   int        `json:"version"`
	DeviceID  string     `json:"device_id"`
	SessionID string     `json:"session_id,omitempty"`
	Hold      ManualHold `json:"hold"`
}

type pendingManualHold struct {
	hold    ManualHold
	cleared bool
}

func manualHoldKey(deviceID string) string {
	return "ev_manual_hold:" + strings.TrimPrefix(sessionKey(deviceID), "ev_session:")
}

// PersistManualHold records an explicit operator action. Before first hardware
// telemetry, persist only a zero-power restriction and bind the actual action
// to the first fresh device reading. A Start still works immediately, but
// cannot grant positive power after restart without matching session proof.
func (m *Manager) PersistManualHold(id string, h ManualHold, cleared bool) (err error) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	defer func() {
		if err != nil {
			m.setManualSaveError(id, true)
		}
	}()
	if m.pendingManual == nil {
		m.pendingManual = map[string]pendingManualHold{}
	}
	m.pendingManual[id] = pendingManualHold{h, cleared}
	return m.flushManualHold(id)
}

func (m *Manager) flushManualHold(id string) (err error) {
	defer func() {
		if err != nil {
			m.setManualSaveError(id, true)
		}
	}()
	pending, ok := m.pendingManual[id]
	if !ok || m.sessionStore == nil {
		return nil
	}
	// One atomic, name-keyed marker is authoritative until the hardware-bound
	// write finishes. It grants no positive manual power, including for OCPP
	// chargers that have not booted or never report a serial. No watts go here.
	restriction := "clear"
	if !pending.cleared && pending.hold.Persistent {
		restriction = "unconfirmed"
		if pending.hold.PowerW == 0 {
			restriction = "pause"
		}
	}
	if err := m.sessionStore.SaveConfig("ev_manual_unbound:"+id, restriction); err != nil {
		return err
	}
	m.mu.RLock()
	lp := m.byID[id]
	var deviceID, sessionID string
	if lp != nil {
		deviceID, sessionID = lp.sessionDeviceID, lp.sessionID
	}
	m.mu.RUnlock()
	if deviceID == "" {
		m.setManualSaveError(id, false)
		return nil
	}
	body := "{}"
	if !pending.cleared && pending.hold.Persistent {
		h := pending.hold
		if !finite(h.PowerW) || h.PowerW < 0 {
			return fmt.Errorf("invalid stored manual power")
		}
		b, err := json.Marshal(storedManualHold{Version: 1, DeviceID: deviceID, SessionID: sessionID, Hold: h})
		if err != nil {
			return err
		}
		body = string(b)
	}
	if err := m.sessionStore.SaveConfig(manualHoldKey(deviceID), body); err != nil {
		return err
	}
	// This name-keyed index is only a hint to pause while identity is unknown;
	// it never grants positive power or bypasses the hardware/session match.
	if err := m.sessionStore.SaveConfig("ev_manual_binding:"+id, deviceID); err != nil {
		return err
	}
	// Retire the legacy name key too: a cleared new record must not fall back
	// to an earlier unbound hold on the next boot.
	if err := m.sessionStore.SaveConfig("loadpoint_manual_hold:"+id, "{}"); err != nil {
		return err
	}
	if err := m.sessionStore.SaveConfig("ev_manual_clear:"+id, ""); err != nil {
		return err
	}
	if err := m.sessionStore.SaveConfig("ev_manual_unbound:"+id, ""); err != nil {
		return err
	}
	delete(m.pendingManual, id)
	m.setManualSaveError(id, false)
	return nil
}

func (m *Manager) setManualSaveError(id string, value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp := m.byID[id]; lp != nil {
		lp.manualSaveError = value
	}
}

// RestoreManualHold returns pending until hardware identity is known. A
// verified same-session positive hold or a hardware-bound pause is restored.
// Unknown/changed sessions and legacy records return a persistent zero-W hold
// plus unconfirmed: failed restoration must not start automatic charging.
func (m *Manager) RestoreManualHold(id string) (ManualHold, string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.RLock()
	lp := m.byID[id]
	var deviceID, sessionID string
	if lp != nil {
		deviceID, sessionID = lp.sessionDeviceID, lp.sessionID
	}
	m.mu.RUnlock()
	if m.sessionStore == nil {
		return ManualHold{}, "none"
	}
	if restriction, _ := m.sessionStore.LoadConfig("ev_manual_unbound:" + id); restriction != "" {
		if restriction == "clear" {
			if m.pendingManual == nil {
				m.pendingManual = map[string]pendingManualHold{}
			}
			m.pendingManual[id] = pendingManualHold{cleared: true}
			_ = m.flushManualHold(id)
			return ManualHold{}, "none"
		}
		if restriction == "pause" {
			return ManualHold{Persistent: true}, "restored"
		}
		return ManualHold{Persistent: true}, "unconfirmed"
	}
	if clear, _ := m.sessionStore.LoadConfig("ev_manual_clear:" + id); clear == "pending" {
		if m.pendingManual == nil {
			m.pendingManual = map[string]pendingManualHold{}
		}
		m.pendingManual[id] = pendingManualHold{cleared: true}
		_ = m.flushManualHold(id)
		return ManualHold{}, "none"
	}
	if deviceID == "" {
		binding, bound := m.sessionStore.LoadConfig("ev_manual_binding:" + id)
		if bound && binding != "" {
			if raw, found := m.sessionStore.LoadConfig(manualHoldKey(binding)); !found || (raw != "" && raw != "{}") {
				return ManualHold{Persistent: true}, "pending"
			}
		}
		legacy, found := m.sessionStore.LoadConfig("loadpoint_manual_hold:" + id)
		if found && legacy != "" && legacy != "{}" {
			return ManualHold{Persistent: true}, "pending"
		}
		return ManualHold{}, "pending"
	}
	raw, found := m.sessionStore.LoadConfig(manualHoldKey(deviceID))
	if !found {
		if binding, ok := m.sessionStore.LoadConfig("ev_manual_binding:" + id); ok && binding != "" {
			if old, present := m.sessionStore.LoadConfig(manualHoldKey(binding)); !present || (old != "" && old != "{}") {
				return ManualHold{Persistent: true}, "unconfirmed"
			}
		}
		legacy, ok := m.sessionStore.LoadConfig("loadpoint_manual_hold:" + id)
		if ok && legacy != "" && legacy != "{}" {
			return ManualHold{Persistent: true}, "unconfirmed"
		}
		return ManualHold{}, "none"
	}
	if raw == "" || raw == "{}" {
		return ManualHold{}, "none"
	}
	var record storedManualHold
	if json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 ||
		record.DeviceID != deviceID || !record.Hold.Persistent ||
		!record.Hold.ExpiresAt.IsZero() || !finite(record.Hold.PowerW) || record.Hold.PowerW < 0 {
		return ManualHold{Persistent: true}, "unconfirmed"
	}
	if record.Hold.PowerW == 0 || (sessionID != "" && sessionID == record.SessionID) {
		return record.Hold, "restored"
	}
	return ManualHold{Persistent: true}, "unconfirmed"
}

func (m *Manager) SetManualRestoreUnconfirmed(id string, value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp := m.byID[id]; lp != nil {
		lp.manualRestoreUnconfirmed = value
	}
}
