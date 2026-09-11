package loadpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/units"
)

// SessionStore is implemented by state.Store. The charger hardware identity,
// never a loadpoint or driver name, keys the saved record.
type SessionStore interface {
	LoadConfig(key string) (string, bool)
	SaveConfig(key, value string) error
}

type savedSession struct {
	Version            int     `json:"version"`
	DeviceID           string  `json:"device_id"`
	SessionID          string  `json:"session_id"`
	AnchorSoC          float64 `json:"anchor_soc"`
	ConfirmedAtWh      float64 `json:"confirmed_at_wh"`
	CapacityWh         float64 `json:"capacity_wh"`
	CompletionNotified bool    `json:"completion_notified,omitempty"`
}

func sessionKey(deviceID string) string {
	h := sha256.Sum256([]byte(deviceID))
	return "ev_session:" + hex.EncodeToString(h[:])
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// SetSessionStore wires durable storage before the controller starts. A
// missing store or hardware session identity leaves SoC usable in memory and
// reports soc_retention=unavailable; it never guesses a prior car's level.
func (m *Manager) SetSessionStore(store SessionStore) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.sessionStore = store
}

// ObserveSession accepts the same reading as Observe plus hardware-issued
// identity. The caller must use fresh telemetry from the currently running
// device. SessionID must identify one physical connection across process
// restart and change after disconnect; missing or ambiguous IDs are empty.
// Endpoint addresses, YAML names and timestamps invented by core are not IDs.
func (m *Manager) ObserveSession(id string, pluggedIn bool, powerW, deliveredWh float64, requestActive bool, deviceID, sessionID string) {
	m.sessionMu.Lock()
	var fired []events.Event
	var bus *events.Bus
	defer func() {
		m.sessionMu.Unlock()
		for _, event := range fired {
			bus.Publish(event)
		}
	}()
	deviceID, sessionID = strings.TrimSpace(deviceID), strings.TrimSpace(sessionID)
	// Endpoint identity can move to another charger without its name changing.
	if strings.HasPrefix(deviceID, "ep:") {
		deviceID = ""
	}
	if !finite(deliveredWh) || deliveredWh < 0 {
		return
	}

	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil {
		m.mu.Unlock()
		return
	}
	previousDevice, previousSession := lp.sessionDeviceID, lp.sessionID
	regressed := pluggedIn && lp.pluggedIn && deliveredWh < lp.deliveredWhSession
	firstSessionProof := deviceID != "" && previousDevice == deviceID && previousSession == "" && sessionID != "" &&
		lp.pluggedIn && pluggedIn && !regressed
	changed := previousDevice != deviceID || (previousSession != sessionID && !firstSessionProof)
	// A changed session can arrive after an unseen unplug while core was
	// offline. Run the ordinary plug-in reset even if connected stayed true.
	if changed || regressed {
		m.nextSessionGeneration++
		lp.sessionGeneration = m.nextSessionGeneration
		lp.pluggedIn = false
		lp.chargingSteadySince = time.Time{}
		lp.stoppedSince = time.Time{}
		lp.steadyRunArmed = false
		if lp.vehicleName != "" || lp.capacityFromCar {
			lp.VehicleCapacityWh = lp.baseCapacityWh
			lp.vehicleName = ""
			lp.capacityFromCar = false
			lp.baseCapacityWh = 0
		}
	}
	lp.sessionDeviceID, lp.sessionID = deviceID, sessionID
	confirmed := lp.socConfirmed && lp.pluggedIn
	m.mu.Unlock()

	if !pluggedIn || regressed {
		// Tombstone the hardware record. A later reconnect cannot resurrect a
		// level from before an observed unplug or a session-counter reset.
		if m.sessionStore != nil {
			for _, knownDevice := range []string{previousDevice, deviceID} {
				if knownDevice != "" {
					_ = m.sessionStore.SaveConfig(sessionKey(knownDevice), "{}")
				}
			}
		}
	}
	var restore *savedSession
	if pluggedIn && !confirmed && !regressed && deviceID != "" && sessionID != "" && m.sessionStore != nil {
		if raw, ok := m.sessionStore.LoadConfig(sessionKey(deviceID)); ok {
			var saved savedSession
			if json.Unmarshal([]byte(raw), &saved) == nil && saved.Version == 1 &&
				saved.DeviceID == deviceID && saved.SessionID == sessionID &&
				finite(saved.AnchorSoC) && finite(saved.ConfirmedAtWh) && saved.ConfirmedAtWh >= 0 &&
				deliveredWh >= saved.ConfirmedAtWh && finite(saved.CapacityWh) && saved.CapacityWh > 0 {
				atConfirmation := saved.AnchorSoC + saved.ConfirmedAtWh/saved.CapacityWh
				if atConfirmation >= 0 && atConfirmation <= 1 {
					restore = &saved
				}
			}
		}
	}
	fired, bus = m.observe(id, pluggedIn, powerW, deliveredWh, requestActive)
	m.mu.Lock()
	lp = m.byID[id]
	if restore != nil && lp.pluggedIn && lp.VehicleCapacityWh == restore.CapacityWh {
		lp.sessionPluginSoC = restore.AnchorSoC
		lp.currentSoC = estimateSoC(restore.AnchorSoC, deliveredWh, restore.CapacityWh)
		lp.socConfirmed = true
		lp.completionNotified = restore.CompletionNotified
		lp.socRetention = "session"
	} else if !lp.socConfirmed || deviceID == "" || sessionID == "" || m.sessionStore == nil {
		lp.socRetention = "unavailable"
	}
	m.mu.Unlock()
	if firstSessionProof && confirmed {
		// The driver can first verify a session when charging starts. Preserve
		// the level the owner entered while waiting and now make it durable.
		m.persistSession(id)
	}
	_ = m.flushManualHold(id)
}

// persistSession runs outside Manager.mu, but sessionMu serializes it with
// observations, unplug and user edits. A slow write cannot block API reads or
// allow an older edit to overwrite a newer one.
func (m *Manager) persistSession(id string) {
	m.mu.RLock()
	lp := m.byID[id]
	if lp == nil {
		m.mu.RUnlock()
		return
	}
	record := savedSession{Version: 1, DeviceID: lp.sessionDeviceID, SessionID: lp.sessionID,
		AnchorSoC: lp.sessionPluginSoC, ConfirmedAtWh: lp.deliveredWhSession,
		CapacityWh: lp.VehicleCapacityWh, CompletionNotified: lp.completionNotified}
	eligible := lp.pluggedIn && lp.socConfirmed && record.DeviceID != "" && record.SessionID != "" &&
		finite(record.AnchorSoC) && finite(record.ConfirmedAtWh) && record.ConfirmedAtWh >= 0 &&
		finite(record.CapacityWh) && record.CapacityWh > 0
	m.mu.RUnlock()
	retention := "unavailable"
	if eligible && m.sessionStore != nil {
		b, err := json.Marshal(record)
		if err == nil {
			err = m.sessionStore.SaveConfig(sessionKey(record.DeviceID), string(b))
		}
		retention = "session"
		if err != nil {
			retention = "error"
		}
	}
	m.mu.Lock()
	if lp := m.byID[id]; lp != nil {
		lp.socRetention = retention
	}
	m.mu.Unlock()
}

// observeConnectionProof handles a lost socket without inventing a cable
// edge or refreshing the last physical observation. A fresh Boot alone must
// not let an older manual Start follow an unidentified connection.
func (m *Manager) observeConnectionProof(id string, generation uint64, unknown bool) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil {
		m.mu.Unlock()
		return
	}
	changed := generation != 0 && lp.connectionGeneration != 0 && generation != lp.connectionGeneration
	if generation != 0 {
		lp.connectionGeneration = generation
	}
	if (!unknown && !changed) || (!changed && lp.sessionDeviceID == "" && lp.sessionID == "") {
		m.mu.Unlock()
		return
	}
	previousDevice := lp.sessionDeviceID
	lp.sessionDeviceID, lp.sessionID = "", ""
	m.nextSessionGeneration++
	lp.sessionGeneration = m.nextSessionGeneration
	lp.socConfirmed = false
	lp.socRetention = "unavailable"
	if lp.vehicleName != "" || lp.capacityFromCar {
		lp.VehicleCapacityWh = lp.baseCapacityWh
		lp.vehicleName = ""
		lp.capacityFromCar = false
		lp.baseCapacityWh = 0
	}
	anchor := lp.PluginSoC
	if anchor <= 0 {
		anchor = units.DefaultPluginSoC
	}
	lp.sessionPluginSoC = anchor
	lp.currentSoC = estimateSoC(anchor, lp.deliveredWhSession, lp.VehicleCapacityWh)
	lp.chargingSteadySince = time.Time{}
	lp.stoppedSince = time.Time{}
	lp.steadyRunArmed = false
	lp.notRequestingSince = time.Time{}
	lp.chargingDeclined = false
	lp.completionNotified = false
	m.mu.Unlock()
	if m.sessionStore != nil && previousDevice != "" {
		_ = m.sessionStore.SaveConfig(sessionKey(previousDevice), "{}")
	}
}
