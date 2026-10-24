package loadpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// savedOffer is FTW's own last order to one charger, never a reading from
// it. A charger keeps that order while Core restarts; without it the first
// tick would count a charging car as idle and stop it near a limit.
type savedOffer struct {
	Version   int       `json:"version"`
	DeviceID  string    `json:"device_id"`
	SessionID string    `json:"session_id,omitempty"`
	SessionWh float64   `json:"session_wh"`
	PowerW    float64   `json:"power_w"`
	PhaseMode string    `json:"phase_mode"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// offerRestoreMaxAge bounds a restart. An older order may have been replaced
// by the charger's own app or schedule.
const offerRestoreMaxAge = 10 * time.Minute

func offerKey(deviceID string) string {
	h := sha256.Sum256([]byte(deviceID))
	return "ev_offer:" + hex.EncodeToString(h[:])
}

// saveOffer keeps the order when it changes, and refreshes it once a minute
// so its session energy and age still describe the running session.
func (c *Controller) saveOffer(now time.Time, cfg Config, sample EVSample, powerW float64, phase, reason string) {
	store := c.manager.sessionStore
	if store == nil || sample.DeviceID == "" || !finite(powerW) {
		return
	}
	if c.savedOffers == nil {
		c.savedOffers = map[string]savedOffer{}
	}
	last, saved := c.savedOffers[cfg.ID]
	if !sample.Connected {
		return
	}
	if saved && last.DeviceID == sample.DeviceID && last.SessionID == sample.SessionID &&
		last.PowerW == powerW && last.PhaseMode == phase && last.Reason == reason && now.Sub(last.At) < time.Minute {
		return
	}
	record := savedOffer{Version: 1, DeviceID: sample.DeviceID, SessionID: sample.SessionID,
		SessionWh: sample.SessionWh, PowerW: powerW, PhaseMode: phase, Reason: reason, At: now}
	if b, err := json.Marshal(record); err == nil && store.SaveConfig(offerKey(record.DeviceID), string(b)) == nil {
		c.savedOffers[cfg.ID] = record
	}
}

// restoreOffers runs before the first command after a restart. It brings back
// our last order only while the same plug-in session is connected: a matching
// session id, or else an energy counter that has not gone back. An observed
// unplug clears the order; a missing reading does not.
func (c *Controller) restoreOffers(now time.Time, samples map[string]EVSample) {
	store := c.manager.sessionStore
	if store == nil {
		return
	}
	for _, cfg := range c.manager.Configs() {
		sample, observed := samples[cfg.DriverName]
		if last, saved := c.savedOffers[cfg.ID]; saved && observed && !sample.Connected && !sample.ConnectionUnknown {
			_ = store.SaveConfig(offerKey(last.DeviceID), "{}")
			delete(c.savedOffers, cfg.ID)
			continue
		}
		st, _ := c.manager.State(cfg.ID)
		if st.CommandedKnown || !sample.Connected || sample.ConnectionUnknown || sample.DeviceID == "" {
			continue
		}
		raw, ok := store.LoadConfig(offerKey(sample.DeviceID))
		var saved savedOffer
		if !ok || json.Unmarshal([]byte(raw), &saved) != nil || saved.Version != 1 || saved.DeviceID != sample.DeviceID ||
			!finite(saved.PowerW) || saved.PowerW < 0 || saved.At.After(now) || now.Sub(saved.At) > offerRestoreMaxAge {
			continue
		}
		sameSession := saved.SessionID != "" && saved.SessionID == sample.SessionID
		if saved.SessionID == "" && sample.SessionID == "" {
			sameSession = !sample.SessionWhUnavailable && saved.SessionWh > 0 && sample.SessionWh >= saved.SessionWh
		}
		if !sameSession {
			continue
		}
		c.manager.setCommandedForManual(cfg.ID, saved.PowerW, saved.Reason, time.Time{})
		c.manager.setCommandedPhases(cfg.ID, c.commandPhases(saved.PhaseMode, saved.PowerW, cfg.PhaseSplitW))
		if c.savedOffers == nil {
			c.savedOffers = map[string]savedOffer{}
		}
		c.savedOffers[cfg.ID] = saved
	}
}
