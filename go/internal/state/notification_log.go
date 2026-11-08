package state

import (
	"fmt"
	"time"
)

// NotificationEntry is one recorded push-notification attempt.
//
// Status is "sent" on success, "failed" on transport or render error, and
// "queued" when only FTW Cloud's uplink holds it, not yet known to have left
// the box. Error holds the backend error text when Status=="failed", empty
// otherwise.
// The table is populated by a main.go subscriber on
// events.KindNotificationDispatched so the notifications package never
// needs to import state.
type NotificationEntry struct {
	ID        int64  `json:"id"`
	TsMs      int64  `json:"ts_ms"`
	EventType string `json:"event_type"`
	Driver    string `json:"driver,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Priority  int    `json:"priority,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// RecordNotification appends one entry to the notification_log table.
// ID is ignored on input (auto-assigned by SQLite); if TsMs is 0 the
// current wall-clock ms is used so callers don't have to thread clocks.
func (s *Store) RecordNotification(e NotificationEntry) error {
	if e.TsMs == 0 {
		e.TsMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(
		`INSERT INTO notification_log
			(ts_ms, event_type, driver, title, body, priority, status, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TsMs, e.EventType, e.Driver, e.Title, e.Body, e.Priority, e.Status, e.Error)
	if err != nil {
		return fmt.Errorf("notification_log insert: %w", err)
	}
	return nil
}

// RecentNotifications returns the most recent `limit` entries, newest first.
// A limit <= 0 defaults to 100; callers should cap externally (the API
// handler caps at 500 to keep responses bounded).
func (s *Store) RecentNotifications(limit int) ([]NotificationEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, ts_ms, event_type, driver, title, body, priority, status, error
			FROM notification_log
			ORDER BY ts_ms DESC
			LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationEntry, 0, limit)
	for rows.Next() {
		var e NotificationEntry
		if err := rows.Scan(&e.ID, &e.TsMs, &e.EventType, &e.Driver, &e.Title,
			&e.Body, &e.Priority, &e.Status, &e.Error); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
