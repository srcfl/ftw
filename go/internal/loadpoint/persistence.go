package loadpoint

import (
	"context"
	"errors"
)

func persistencePending(err error) bool {
	var pending interface{ Pending() bool }
	return errors.As(err, &pending) && pending.Pending()
}

// WaitForPersistence is for acknowledgements, outside every manager and
// controller lock. The control loop must never wait for a disk write.
func (m *Manager) WaitForPersistence(ctx context.Context) error {
	m.sessionMu.Lock()
	store := m.sessionStore
	m.sessionMu.Unlock()
	if waiter, ok := store.(interface{ Flush(context.Context) error }); ok {
		return waiter.Flush(ctx)
	}
	return nil
}

// Caller holds Manager.mu. The optional status reader is memory-only.
func (m *Manager) snapshot(lp *loadpointRuntime) State {
	st := lp.snapshot()
	if writer, ok := m.sessionStore.(interface{ GroupStatus(string) error }); ok {
		retention := func(group string) string {
			err := writer.GroupStatus(group)
			if err == nil {
				return "session"
			}
			if persistencePending(err) {
				return "pending"
			}
			return "error"
		}
		if st.SoCRetention == "pending" {
			st.SoCRetention = retention(sessionKey(lp.sessionDeviceID))
		}
		if st.GoalRetention == "pending" {
			st.GoalRetention = retention(finishGoalKey(lp.sessionDeviceID))
		}
		if st.ManualSavePending {
			status := retention("manual:" + lp.ID)
			st.ManualSavePending = status == "pending"
			st.ManualSaveError = status == "error"
		}
	}
	return st
}
