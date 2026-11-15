package state

import (
	"context"
	"fmt"
)

// LogDigest is one warning or error pattern as logged within one hour: how
// often, when first and last, and the latest full text. The support file
// reads a week of these. The in-memory log ring cannot hold that long: a
// driver logging every control tick filled it in 75 minutes on a real site,
// and a restart empties it.
type LogDigest struct {
	HourMs int64  `json:"hour_ms"`
	Level  string `json:"level"`
	Driver string `json:"driver,omitempty"`
	// Pattern is the message with its numbers folded, the key that groups
	// repeats whose figures differ.
	Pattern string `json:"pattern"`
	Msg     string `json:"msg"`
	Attrs   string `json:"attrs,omitempty"`
	Count   int64  `json:"count"`
	FirstMs int64  `json:"first_ms"`
	LastMs  int64  `json:"last_ms"`
}

// AddLogDigest adds counts to the stored rows. A row seen again in the same
// hour keeps its first sighting and takes the latest text.
func (s *Store) AddLogDigest(ctx context.Context, rows []LogDigest) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("log digest: begin: %w", err)
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO log_digest
			(hour_ms, level, driver, pattern, msg, attrs, count, first_ms, last_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (hour_ms, level, driver, pattern) DO UPDATE SET
			  count = count + excluded.count,
			  first_ms = MIN(first_ms, excluded.first_ms),
			  msg = CASE WHEN excluded.last_ms >= last_ms THEN excluded.msg ELSE msg END,
			  attrs = CASE WHEN excluded.last_ms >= last_ms THEN excluded.attrs ELSE attrs END,
			  last_ms = MAX(last_ms, excluded.last_ms)`,
			r.HourMs, r.Level, r.Driver, r.Pattern, r.Msg, r.Attrs, r.Count, r.FirstMs, r.LastMs); err != nil {
			return fmt.Errorf("log digest: add: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("log digest: commit: %w", err)
	}
	return nil
}

// LogDigestBetween returns the rows whose hour starts in [sinceMs, untilMs],
// oldest first, and whether more matched than the limit allowed. The limit
// keeps the newest.
func (s *Store) LogDigestBetween(ctx context.Context, sinceMs, untilMs int64, limit int) ([]LogDigest, bool, error) {
	if limit <= 0 {
		limit = 1000
	}
	// A row's hour may begin before sinceMs and still hold sightings inside.
	rows, err := s.db.QueryContext(ctx, `SELECT hour_ms, level, driver, pattern, msg, attrs, count, first_ms, last_ms
		FROM log_digest WHERE hour_ms <= ? AND last_ms >= ?
		ORDER BY hour_ms DESC, first_ms DESC LIMIT ?`, untilMs, sinceMs, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("log digest: query: %w", err)
	}
	defer rows.Close()
	out := []LogDigest{}
	for rows.Next() {
		var r LogDigest
		if err := rows.Scan(&r.HourMs, &r.Level, &r.Driver, &r.Pattern, &r.Msg, &r.Attrs, &r.Count, &r.FirstMs, &r.LastMs); err != nil {
			return nil, false, fmt.Errorf("log digest: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("log digest: rows: %w", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, more, nil
}

// PruneLogDigest deletes the hours that began before beforeMs.
func (s *Store) PruneLogDigest(ctx context.Context, beforeMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM log_digest WHERE hour_ms < ?`, beforeMs)
	if err != nil {
		return 0, fmt.Errorf("log digest: prune: %w", err)
	}
	return res.RowsAffected()
}
