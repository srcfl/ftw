package state

import (
	"context"
	"fmt"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

// ForecastIssueCursor orders equal timestamps by ID. A page is fully decoded
// and its read cursor closed before callers start a score write transaction.
type ForecastIssueCursor struct {
	IssuedAtMS int64
	ID         string
}

func (s *Store) LoadForecastScorePage(ctx context.Context, since, until int64, after ForecastIssueCursor, limit int) ([]forecasting.Issue, ForecastIssueCursor, error) {
	if limit < 1 || limit > 16 {
		return nil, after, fmt.Errorf("forecast score page limit must be 1..16")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM forecast_issues
 WHERE issued_at_ms>=? AND issued_at_ms<=?
 AND (issued_at_ms>? OR (issued_at_ms=? AND id>?))
 ORDER BY issued_at_ms,id LIMIT ?`, since, until, after.IssuedAtMS, after.IssuedAtMS, after.ID, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	issues := make([]forecasting.Issue, 0, limit)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, after, err
		}
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, after, err
		}
		issue, _, err := decodeForecastIssue(data)
		if err != nil {
			return nil, after, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, after, err
	}
	if len(issues) > 0 {
		last := issues[len(issues)-1]
		after = ForecastIssueCursor{IssuedAtMS: last.IssuedAtMS, ID: last.ID}
	}
	return issues, after, nil
}
