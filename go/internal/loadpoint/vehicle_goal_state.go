package loadpoint

import (
	"encoding/json"
	"time"
)

type savedFinishGoal struct {
	DeviceID  string    `json:"device_id"`
	SessionID string    `json:"session_id"`
	Deadline  time.Time `json:"deadline"`
	Schedule  Schedule  `json:"schedule"`
	Completed bool      `json:"completed,omitempty"`
}

func finishGoalKey(device string) string { return "ev_finish:" + sessionKey(device) }

// Called under sessionMu after fresh charger identity has been observed. The
// deadline belongs to that physical connection, not to a guessed car SoC.
func (m *Manager) retainFinishGoal(id string) {
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil || !lp.pluggedIn || !lp.finishAtVehicleLimit {
		m.mu.Unlock()
		return
	}
	if lp.sessionDeviceID == "" || lp.sessionID == "" || m.sessionStore == nil {
		lp.finishGoalRetention = "unavailable"
		m.mu.Unlock()
		return
	}
	record := savedFinishGoal{lp.sessionDeviceID, lp.sessionID, lp.targetTime, lp.schedule, lp.finishGoalCompleted}
	check := !lp.finishGoalChecked && !lp.finishGoalExplicit
	lp.finishGoalChecked = true
	m.mu.Unlock()
	if check {
		if raw, ok := m.sessionStore.LoadConfig(finishGoalKey(record.DeviceID)); ok {
			var saved savedFinishGoal
			if json.Unmarshal([]byte(raw), &saved) == nil && saved.DeviceID == record.DeviceID && (saved.SessionID == record.SessionID || (saved.Completed && !record.Schedule.Recurring)) && saved.Schedule == record.Schedule && !saved.Deadline.IsZero() {
				m.mu.Lock()
				if lp.finishGoalExplicit || !lp.finishAtVehicleLimit || lp.schedule != record.Schedule || lp.targetTime != record.Deadline {
					m.mu.Unlock()
					return
				}
				record.Deadline = saved.Deadline
				record.Completed = saved.Completed
				lp.finishGoalCompleted = saved.Completed
				lp.finishGoalSavedCompleted = saved.Completed
				if saved.Completed {
					lp.targetSoC = 0
				}
				lp.targetTime = saved.Deadline
				lp.lastRolledFor = saved.Deadline
				lp.finishGoalSaved = saved.Deadline
				lp.finishGoalRetention = "session"
				m.mu.Unlock()
			}
		}
	}
	m.mu.RLock()
	due := !record.Deadline.IsZero() && (lp.finishGoalSaved != record.Deadline || lp.finishGoalSavedCompleted != record.Completed)
	m.mu.RUnlock()
	if !due {
		return
	}
	raw, err := json.Marshal(record)
	if err == nil {
		err = m.sessionStore.SaveConfig(finishGoalKey(record.DeviceID), string(raw))
	}
	m.mu.Lock()
	if err != nil {
		lp.finishGoalRetention = "error"
		if persistencePending(err) {
			lp.finishGoalRetention = "pending"
		}
	} else {
		lp.finishGoalSaved = record.Deadline
		lp.finishGoalSavedCompleted = record.Completed
		lp.finishGoalRetention = "session"
	}
	m.mu.Unlock()
}

// A one-shot goal stays finished across restart and later plug sessions.
// A recurring goal can roll to the next deadline once this one is complete.
func (m *Manager) completeVehicleGoal(id string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil || !lp.finishAtVehicleLimit || !lp.pluggedIn {
		m.mu.Unlock()
		return
	}
	lp.finishGoalCompleted = true
	lp.targetSoC = 0
	m.mu.Unlock()
	m.retainFinishGoal(id)
}

// Fresh evidence of renewed demand reopens a recurring goal, for example
// when the owner raises the car's limit while it stays connected.
func (m *Manager) resumeRecurringVehicleGoal(id string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil || !lp.finishAtVehicleLimit || !lp.schedule.Recurring || !lp.pluggedIn {
		m.mu.Unlock()
		return
	}
	lp.finishGoalCompleted = false
	lp.targetSoC = 1
	m.mu.Unlock()
	m.retainFinishGoal(id)
}
