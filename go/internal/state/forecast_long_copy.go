package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

const (
	forecastLongIssuePage = 16
	forecastLongObsPage   = 128
)

// forecastLongAfterIssuePage runs after a committed issue page. Tests use it
// to stop a copy between pages.
var forecastLongAfterIssuePage func(rows int) error

type forecastLongIssueCursor struct {
	IssuedAtMS int64  `json:"issued_at_ms"`
	ID         string `json:"id"`
}

type forecastLongObsCursor struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Config  string `json:"config_version"`
}

// Sync copies the 30-day issue and observation tables, then at most once a
// day thins, prunes and gives free pages back. Failures stay in the published
// status. A full disk pauses until the next call instead of retrying.
func (a *ForecastLongArchive) Sync(ctx context.Context, src *Store, now time.Time) error {
	if a == nil || src == nil {
		return errors.New("forecast archive: copy source unavailable")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	a.mu.Lock()
	a.status.LastError = ""
	a.mu.Unlock()
	if err := src.InitForecastArchive(ctx); err != nil {
		a.fail("schema", err)
		return err
	}
	var paused error
	if !a.blocked() {
		if err := a.copyIssues(ctx, src); err != nil {
			if forecastLongPaused(err) {
				paused = err
			} else {
				a.fail("copy-issues", err)
				return err
			}
		}
	}
	if paused == nil && !a.blocked() {
		if err := a.copyObservations(ctx, src); err != nil {
			if forecastLongPaused(err) {
				paused = err
			} else {
				a.fail("copy-observations", err)
				return err
			}
		}
	}
	if paused == nil {
		due, err := a.maintenanceDue(ctx, now)
		if err != nil {
			a.fail("maintenance", err)
			return err
		}
		if due {
			if err := a.maintain(ctx, now); err != nil {
				if forecastLongPaused(err) {
					paused = err
				} else {
					a.fail("maintenance", err)
					return err
				}
			} else if err := a.setMeta(ctx, "last_maintain_ms", jsonInt(now.UnixMilli())); err != nil {
				a.fail("maintenance", err)
				return err
			}
		}
	}
	if err := a.recompute(ctx, src, now, paused); err != nil {
		a.fail("status", err)
		return err
	}
	return nil
}

func forecastLongPaused(err error) bool {
	return errors.Is(err, errForecastArchiveDiskFull) || sqliteFull(err)
}

func (a *ForecastLongArchive) blocked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status.State == "over_cap"
}

func (a *ForecastLongArchive) fail(kind string, err error) {
	a.warn(kind, err)
	a.mu.Lock()
	if a.status.State == "" || a.status.State == "starting" || a.status.State == "ok" {
		a.status.State = "unavailable"
	}
	a.mu.Unlock()
	a.publish()
}

func (a *ForecastLongArchive) copyIssues(ctx context.Context, src *Store) error {
	cursor, err := a.issueCursor(ctx)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageCtx, cancel := context.WithTimeout(ctx, forecastLongPageTimeout)
		var page []ForecastIssueCopy
		err = src.VisitForecastIssuesAfter(pageCtx, cursor.IssuedAtMS, cursor.ID, forecastLongIssuePage, func(item ForecastIssueCopy) error {
			page = append(page, item)
			return nil
		})
		cancel()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		batch := make([]ForecastArchiveIssue, 0, len(page))
		for _, item := range page {
			cursor = forecastLongIssueCursor{IssuedAtMS: item.IssuedAtMS, ID: item.ID}
			if item.Err != nil {
				a.warn("decode", item.Err)
				continue
			}
			states, err := forecastLongStatesFrom(ctx, src, item.Issue)
			if err != nil {
				return err
			}
			batch = append(batch, ForecastArchiveIssue{
				Issue: item.Issue, JSONBytes: item.JSONBytes, GzipBytes: item.GzipBytes, States: states,
			})
		}
		writeCtx, cancelWrite := context.WithTimeout(ctx, forecastLongPageTimeout)
		err = a.ArchiveIssues(writeCtx, batch)
		if err == nil || errors.Is(err, errForecastArchiveImmutable) {
			err = a.writeIssueCursor(writeCtx, cursor)
		}
		cancelWrite()
		if err != nil {
			return err
		}
		if forecastLongAfterIssuePage != nil {
			if err := forecastLongAfterIssuePage(len(page)); err != nil {
				return err
			}
		}
		a.mu.Lock()
		a.status.LastCopyMS = time.Now().UnixMilli()
		a.mu.Unlock()
		if len(page) < forecastLongIssuePage {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func forecastLongStatesFrom(ctx context.Context, src *Store, issue forecasting.Issue) (map[string]ForecastModelBlob, error) {
	states := map[string]ForecastModelBlob{}
	for _, model := range issue.Models {
		if model.StateID == "" {
			continue
		}
		if _, ok := states[model.StateID]; ok {
			continue
		}
		expanded, payload, err := src.LoadForecastModelStateRaw(ctx, model.StateID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		states[model.StateID] = ForecastModelBlob{Expanded: expanded, Gzip: payload}
	}
	return states, nil
}

func (a *ForecastLongArchive) copyObservations(ctx context.Context, src *Store) error {
	cursor, err := a.obsCursor(ctx)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageCtx, cancel := context.WithTimeout(ctx, forecastLongPageTimeout)
		var page []forecasting.Observation
		err = src.VisitForecastObservationsAfter(pageCtx, cursor.StartMS, cursor.EndMS, cursor.Config, forecastLongObsPage, func(row forecasting.Observation) error {
			page = append(page, row)
			return nil
		})
		cancel()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		writeCtx, cancelWrite := context.WithTimeout(ctx, forecastLongPageTimeout)
		err = a.ArchiveObservations(writeCtx, page)
		if err == nil {
			last := page[len(page)-1]
			cursor = forecastLongObsCursor{StartMS: last.StartMS, EndMS: last.EndMS, Config: last.ConfigVersion}
			err = a.writeObsCursor(writeCtx, cursor)
		}
		cancelWrite()
		if err != nil {
			return err
		}
		if len(page) < forecastLongObsPage {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) issueCursor(ctx context.Context) (forecastLongIssueCursor, error) {
	var cursor forecastLongIssueCursor
	raw, err := a.metaValue(ctx, "copy_cursor_issue")
	if err != nil || raw == "" {
		return cursor, err
	}
	err = json.Unmarshal([]byte(raw), &cursor)
	return cursor, err
}

func (a *ForecastLongArchive) writeIssueCursor(ctx context.Context, cursor forecastLongIssueCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	return a.setMeta(ctx, "copy_cursor_issue", string(data))
}

func (a *ForecastLongArchive) obsCursor(ctx context.Context) (forecastLongObsCursor, error) {
	var cursor forecastLongObsCursor
	raw, err := a.metaValue(ctx, "copy_cursor_obs")
	if err != nil || raw == "" {
		return cursor, err
	}
	err = json.Unmarshal([]byte(raw), &cursor)
	return cursor, err
}

func (a *ForecastLongArchive) writeObsCursor(ctx context.Context, cursor forecastLongObsCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	return a.setMeta(ctx, "copy_cursor_obs", string(data))
}

func (a *ForecastLongArchive) metaValue(ctx context.Context, key string) (string, error) {
	var value string
	err := a.db.QueryRowContext(ctx, `SELECT value FROM fa_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (a *ForecastLongArchive) setMeta(ctx context.Context, key, value string) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO fa_meta(key, value) VALUES(?, ?)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (a *ForecastLongArchive) maintenanceDue(ctx context.Context, now time.Time) (bool, error) {
	raw, err := a.metaValue(ctx, "last_maintain_ms")
	if err != nil || raw == "" {
		return err == nil, err
	}
	var ms int64
	if err := json.Unmarshal([]byte(raw), &ms); err != nil {
		return true, nil
	}
	return now.Sub(time.UnixMilli(ms)) >= a.maintainEvery, nil
}

func jsonInt(v int64) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func (src *Store) forecastCopyLag(ctx context.Context, cursor forecastLongIssueCursor, now int64) (int64, error) {
	var issued sql.NullInt64
	err := src.db.QueryRowContext(ctx, `SELECT MIN(issued_at_ms) FROM forecast_issues
 WHERE issued_at_ms > ? OR (issued_at_ms = ? AND id > ?)`, cursor.IssuedAtMS, cursor.IssuedAtMS, cursor.ID).Scan(&issued)
	if err != nil || !issued.Valid {
		return 0, err
	}
	lag := now - issued.Int64
	if lag < 0 {
		lag = 0
	}
	return lag, nil
}
