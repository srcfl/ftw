package state

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// ControlSpan is one stretch of time in which a subject held one verdict:
// a device function's answer to "Are we in control?", a source's freshness,
// whether the site may dispatch, the operating mode, or one plan slot's
// delivery. The journal opens a span when the verdict changes and closes the
// one before it, so the spans of a subject tile its history.
type ControlSpan struct {
	ID int64 `json:"id"`
	// Subject names what held the verdict, e.g. "battery:sungrow",
	// "source:easee", "site:dispatch", "site:mode" or "slot:batteries".
	Subject  string `json:"subject"`
	Driver   string `json:"driver,omitempty"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Severity string `json:"severity,omitempty"`
	Mode     string `json:"mode,omitempty"`
	StartMs  int64  `json:"start_ms"`
	// EndMs is 0 while the verdict still holds.
	EndMs     int64 `json:"end_ms,omitempty"`
	UpdatedMs int64 `json:"updated_ms"`
	Samples   int64 `json:"samples"`
	// Requested and Actual summarise the power Core asked for and the
	// device delivered over the span. A sample without a number adds
	// nothing, so N may be below Samples.
	Requested PowerStats `json:"requested_w"`
	Actual    PowerStats `json:"actual_w"`
	// First and Last are the verdict as Core served it when the span
	// opened and at its latest sample.
	First json.RawMessage `json:"first,omitempty"`
	Last  json.RawMessage `json:"last,omitempty"`
}

// PowerStats sums the watts seen over a span.
type PowerStats struct {
	N   int64   `json:"n"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Sum float64 `json:"sum"`
}

// Add takes one reading; nil or a value that is not a finite number adds
// nothing.
func (p *PowerStats) Add(w *float64) {
	if w == nil || math.IsNaN(*w) || math.IsInf(*w, 0) {
		return
	}
	if p.N == 0 || *w < p.Min {
		p.Min = *w
	}
	if p.N == 0 || *w > p.Max {
		p.Max = *w
	}
	p.N++
	p.Sum += *w
}

// Mean is the average reading, false when there was none.
func (p PowerStats) Mean() (float64, bool) {
	if p.N == 0 {
		return 0, false
	}
	return p.Sum / float64(p.N), true
}

// ControlSpanQuery picks the spans that overlap [SinceMs, UntilMs].
type ControlSpanQuery struct {
	SinceMs int64
	UntilMs int64
	// SubjectPrefix keeps subjects that start with it, e.g. "battery:".
	SubjectPrefix string
	Driver        string
	// Severities keeps these severities; empty keeps all.
	Severities []string
	// Limit bounds the answer; the newest spans come first.
	Limit int
}

const controlJournalColumns = `id, subject, driver, kind, status, reason, severity, mode,
	start_ms, COALESCE(end_ms, 0), updated_ms, samples,
	requested_n, COALESCE(requested_min, 0), COALESCE(requested_max, 0), requested_sum,
	actual_n, COALESCE(actual_min, 0), COALESCE(actual_max, 0), actual_sum,
	first_json, last_json`

// SaveControlSpans writes new spans and updates known ones in one
// transaction. A span with ID 0 is inserted and gets its ID only after the
// commit, so a failed write leaves every span as it was for the next try.
func (s *Store) SaveControlSpans(ctx context.Context, spans []*ControlSpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("control journal: begin: %w", err)
	}
	defer tx.Rollback()
	ids := make([]int64, len(spans))
	for i, sp := range spans {
		args := []any{
			sp.Subject, sp.Driver, sp.Kind, sp.Status, sp.Reason, sp.Severity, sp.Mode,
			sp.StartMs, nullableMs(sp.EndMs), sp.UpdatedMs, sp.Samples,
			sp.Requested.N, nullableStat(sp.Requested, sp.Requested.Min), nullableStat(sp.Requested, sp.Requested.Max), sp.Requested.Sum,
			sp.Actual.N, nullableStat(sp.Actual, sp.Actual.Min), nullableStat(sp.Actual, sp.Actual.Max), sp.Actual.Sum,
			jsonText(sp.First), jsonText(sp.Last),
		}
		if sp.ID == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO control_journal
				(subject, driver, kind, status, reason, severity, mode,
				 start_ms, end_ms, updated_ms, samples,
				 requested_n, requested_min, requested_max, requested_sum,
				 actual_n, actual_min, actual_max, actual_sum,
				 first_json, last_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
			if err != nil {
				return fmt.Errorf("control journal: insert %s: %w", sp.Subject, err)
			}
			if ids[i], err = res.LastInsertId(); err != nil {
				return fmt.Errorf("control journal: insert id: %w", err)
			}
			continue
		}
		// first_json never changes after the insert, so an update leaves it.
		update := append(append([]any{}, args[:len(args)-2]...), args[len(args)-1], sp.ID)
		if _, err := tx.ExecContext(ctx, `UPDATE control_journal SET
				subject = ?, driver = ?, kind = ?, status = ?, reason = ?, severity = ?, mode = ?,
				start_ms = ?, end_ms = ?, updated_ms = ?, samples = ?,
				requested_n = ?, requested_min = ?, requested_max = ?, requested_sum = ?,
				actual_n = ?, actual_min = ?, actual_max = ?, actual_sum = ?,
				last_json = ?
				WHERE id = ?`, update...); err != nil {
			return fmt.Errorf("control journal: update %d: %w", sp.ID, err)
		}
		ids[i] = sp.ID
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("control journal: commit: %w", err)
	}
	for i, sp := range spans {
		sp.ID = ids[i]
	}
	return nil
}

// ControlSpans returns the spans that overlap the query's window, newest
// first, and whether more matched than the limit allowed.
func (s *Store) ControlSpans(ctx context.Context, q ControlSpanQuery) ([]ControlSpan, bool, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	where := []string{"start_ms <= ?", "(end_ms IS NULL OR end_ms >= ?)"}
	args := []any{q.UntilMs, q.SinceMs}
	if q.SubjectPrefix != "" {
		where = append(where, "substr(subject, 1, ?) = ?")
		args = append(args, len(q.SubjectPrefix), q.SubjectPrefix)
	}
	if q.Driver != "" {
		where = append(where, "driver = ?")
		args = append(args, q.Driver)
	}
	if len(q.Severities) > 0 {
		where = append(where, "severity IN (?"+strings.Repeat(", ?", len(q.Severities)-1)+")")
		for _, sev := range q.Severities {
			args = append(args, sev)
		}
	}
	args = append(args, q.Limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+controlJournalColumns+`
		FROM control_journal WHERE `+strings.Join(where, " AND ")+`
		ORDER BY start_ms DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("control journal: query: %w", err)
	}
	defer rows.Close()
	out := []ControlSpan{}
	for rows.Next() {
		var sp ControlSpan
		var first, last string
		if err := rows.Scan(&sp.ID, &sp.Subject, &sp.Driver, &sp.Kind, &sp.Status, &sp.Reason, &sp.Severity, &sp.Mode,
			&sp.StartMs, &sp.EndMs, &sp.UpdatedMs, &sp.Samples,
			&sp.Requested.N, &sp.Requested.Min, &sp.Requested.Max, &sp.Requested.Sum,
			&sp.Actual.N, &sp.Actual.Min, &sp.Actual.Max, &sp.Actual.Sum,
			&first, &last); err != nil {
			return nil, false, fmt.Errorf("control journal: scan: %w", err)
		}
		sp.First, sp.Last = rawJSON(first), rawJSON(last)
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("control journal: rows: %w", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// CloseOpenControlSpans ends every span a previous run left open at the
// last moment that run saw it hold. Nothing is known after that, so the
// span does not run on through the restart.
func (s *Store) CloseOpenControlSpans(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE control_journal SET end_ms = updated_ms WHERE end_ms IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("control journal: close open spans: %w", err)
	}
	return res.RowsAffected()
}

// PruneControlSpans deletes spans that ended before beforeMs.
func (s *Store) PruneControlSpans(ctx context.Context, beforeMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM control_journal WHERE end_ms IS NOT NULL AND end_ms < ?`, beforeMs)
	if err != nil {
		return 0, fmt.Errorf("control journal: prune: %w", err)
	}
	return res.RowsAffected()
}

// NotificationsBetween returns notification attempts in [sinceMs, untilMs],
// newest first, for one driver when driver is set, and whether more matched
// than the limit allowed.
func (s *Store) NotificationsBetween(ctx context.Context, sinceMs, untilMs int64, driver string, limit int) ([]NotificationEntry, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts_ms, event_type, driver, title, body, priority, status, error
			FROM notification_log
			WHERE ts_ms >= ? AND ts_ms <= ? AND (? = '' OR driver = ?)
			ORDER BY ts_ms DESC, id DESC
			LIMIT ?`, sinceMs, untilMs, driver, driver, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []NotificationEntry{}
	for rows.Next() {
		var e NotificationEntry
		if err := rows.Scan(&e.ID, &e.TsMs, &e.EventType, &e.Driver, &e.Title,
			&e.Body, &e.Priority, &e.Status, &e.Error); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

func nullableMs(ms int64) any {
	if ms == 0 {
		return nil
	}
	return ms
}

func nullableStat(p PowerStats, v float64) any {
	if p.N == 0 {
		return nil
	}
	return v
}

func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return "{}"
	}
	return string(raw)
}

func rawJSON(text string) json.RawMessage {
	if text == "" || text == "{}" {
		return nil
	}
	return json.RawMessage(text)
}
