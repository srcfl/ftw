package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const vehicleWakeGap = 90 * time.Second
const vehicleWakeWindow = 30 * time.Minute
const vehicleWakeBudget = 3

type vehicleWakeCheckpoint struct {
	WindowStartMs int64 `json:"window_start_ms"`
	LastAttemptMs int64 `json:"last_attempt_ms"`
	Attempts      int   `json:"attempts"`
}

// ReserveVehicleWake commits the attempt before any network request. A crash,
// driver rename or failed HTTP call cannot restore the wake budget. This uses
// existing config rows and does not change the state schema.
func (s *Store) ReserveVehicleWake(deviceID string, now time.Time) (bool, time.Duration, error) {
	if deviceID == "" || now.IsZero() {
		return false, vehicleWakeWindow, fmt.Errorf("vehicle wake requires hardware identity and time")
	}
	allowed := false
	retry := vehicleWakeWindow
	err := s.durableConfigWrite(func(tx *sql.Tx) error {
		key := "vehicle_wake:" + deviceID
		var raw string
		c := vehicleWakeCheckpoint{}
		err := tx.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal([]byte(raw), &c); err != nil {
				return err
			}
			if c.Attempts < 1 || c.Attempts > vehicleWakeBudget || c.WindowStartMs <= 0 || c.LastAttemptMs < c.WindowStartMs {
				return fmt.Errorf("invalid vehicle wake checkpoint")
			}
		}
		nowMs := now.UnixMilli()
		windowEnd := time.UnixMilli(c.WindowStartMs).Add(vehicleWakeWindow)
		// Clock rollback retains the budget instead of treating the record as absent.
		if c.Attempts > 0 && now.Before(windowEnd) {
			next := time.UnixMilli(c.LastAttemptMs).Add(vehicleWakeGap)
			if c.Attempts >= vehicleWakeBudget {
				next = windowEnd
			}
			if now.Before(next) {
				retry = next.Sub(now)
				return nil
			}
		} else {
			c = vehicleWakeCheckpoint{WindowStartMs: nowMs}
		}
		c.Attempts++
		c.LastAttemptMs = nowMs
		encoded, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := saveConfigValues(tx, map[string]string{key: string(encoded)}); err != nil {
			return err
		}
		allowed, retry = true, vehicleWakeGap
		return nil
	})
	if err != nil {
		return false, vehicleWakeWindow, err
	}
	return allowed, retry, nil
}
