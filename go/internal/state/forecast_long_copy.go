package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

const (
	forecastLongIssuePage = 16
	forecastLongObsPage   = 128
	// forecastLongCopyOverlap is how far before its cursor each copy reads
	// again. A row that commits late with a slightly earlier key, such as a
	// shadow issue saved after its primary or a write after a small clock
	// step back, is still copied. It must be at least maxForecastFutureSkew:
	// a cursor never runs further ahead of the clock than that, so every row
	// written after a step back sorts after cursor minus overlap.
	forecastLongCopyOverlap = time.Hour
)

// forecastLongAfterIssuePage runs after a committed issue page. Tests use it
// to stop a copy between pages.
var forecastLongAfterIssuePage func(rows int) error

type forecastLongIssueCursor struct {
	IssuedAtMS int64  `json:"issued_at_ms"`
	ID         string `json:"id"`
}

func (c forecastLongIssueCursor) after(o forecastLongIssueCursor) bool {
	return c.IssuedAtMS > o.IssuedAtMS || (c.IssuedAtMS == o.IssuedAtMS && c.ID > o.ID)
}

// forecastLongObsCursor follows available_at_ms, the time a quarter was
// released. The tracker writes observations in that order, while a quarter
// with an earlier start can be released after a later one.
type forecastLongObsCursor struct {
	AvailableMS int64  `json:"available_at_ms"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Config      string `json:"config_version"`
}

func (c forecastLongObsCursor) after(o forecastLongObsCursor) bool {
	if c.AvailableMS != o.AvailableMS {
		return c.AvailableMS > o.AvailableMS
	}
	if c.StartMS != o.StartMS {
		return c.StartMS > o.StartMS
	}
	if c.EndMS != o.EndMS {
		return c.EndMS > o.EndMS
	}
	return c.Config > o.Config
}

// forecastLongSourceError marks a failed read of state.db. Damage there is
// healed at boot and must not send the archive to quarantine.
type forecastLongSourceError struct{ err error }

func (e forecastLongSourceError) Error() string { return "state.db: " + e.err.Error() }
func (e forecastLongSourceError) Unwrap() error { return e.err }

func fromForecastSource(err error) error {
	if err == nil {
		return nil
	}
	return forecastLongSourceError{err}
}

// forecastLongDamaged reports a corruption code from the archive itself.
func forecastLongDamaged(err error) bool {
	var source forecastLongSourceError
	return sqliteCorrupt(err) && !errors.As(err, &source)
}

// Sync copies the 30-day issue and observation tables and holds the archive
// to its cap. Once a day it also thins, prunes expired runs and gives free
// pages back. Failures stay in the published status. Below the disk floor
// copying pauses until the next call, but the deletes still run. When the
// archive reports damage, Sync closes it, so the next copy reopens the file,
// runs quick_check and heals it without a restart.
func (a *ForecastLongArchive) Sync(ctx context.Context, src *Store, now time.Time) error {
	err := a.syncOnce(ctx, src, now)
	if forecastLongDamaged(err) && a.store != nil {
		slog.Warn("forecast long archive: damage found while copying; the next copy checks the file", "err", err)
		a.store.dropForecastLongArchive(a)
	}
	return err
}

func (a *ForecastLongArchive) syncOnce(ctx context.Context, src *Store, now time.Time) error {
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
		err = fromForecastSource(err)
		a.fail("schema", err)
		return err
	}
	var paused error
	if !a.blocked() {
		if err := a.copyIssues(ctx, src, now); err != nil {
			if forecastLongPaused(err) {
				paused = err
			} else {
				a.fail("copy-issues", err)
				return err
			}
		}
	}
	if paused == nil && !a.blocked() {
		if err := a.copyObservations(ctx, src, now); err != nil {
			if forecastLongPaused(err) {
				paused = err
			} else {
				a.fail("copy-observations", err)
				return err
			}
		}
	}
	due, err := a.maintenanceDue(ctx, now)
	if err != nil {
		a.fail("maintenance", err)
		return err
	}
	if due {
		err = a.maintain(ctx, now)
		if err == nil {
			err = a.setMeta(ctx, "last_maintain_ms", jsonInt(now.UnixMilli()))
		}
	} else {
		err = a.enforceCap(ctx, now)
	}
	if err != nil {
		if !forecastLongPaused(err) {
			a.fail("maintenance", err)
			return err
		}
		if paused == nil {
			paused = err
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

// copyIssues reads from an hour before its cursor up to the clock plus
// maxForecastFutureSkew. A run already archived is skipped by id before its
// payload is read, so the overlap and a full rescan stay cheap. A cursor
// further ahead than that bound came from a wrong clock; the copy then
// starts again from the beginning of the 30-day table.
func (a *ForecastLongArchive) copyIssues(ctx context.Context, src *Store, now time.Time) error {
	until := now.UnixMilli() + maxForecastFutureSkew.Milliseconds()
	cursor, err := a.issueCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.IssuedAtMS > until {
		slog.Warn("forecast long archive: issue cursor is ahead of the clock; copying the 30-day table again",
			"cursor_ms", cursor.IssuedAtMS, "now_ms", now.UnixMilli())
		cursor = forecastLongIssueCursor{}
		if err := a.writeIssueCursor(ctx, cursor); err != nil {
			return err
		}
	}
	from := forecastLongIssueCursor{IssuedAtMS: cursor.IssuedAtMS - forecastLongCopyOverlap.Milliseconds()}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := a.readIssuePage(ctx, src, from, until)
		if err != nil {
			return err
		}
		if batch.keys == 0 {
			return nil
		}
		from = batch.last
		advanced := from.after(cursor)
		writeCtx, cancelWrite := context.WithTimeout(ctx, forecastLongPageTimeout)
		err = a.ArchiveIssues(writeCtx, batch.issues)
		if errors.Is(err, errForecastArchiveImmutable) {
			err = nil // warned; the stored run stays
		}
		if err == nil && advanced {
			cursor = from
			err = a.writeIssueCursor(writeCtx, cursor)
		}
		cancelWrite()
		if err != nil {
			return err
		}
		if forecastLongAfterIssuePage != nil {
			if err := forecastLongAfterIssuePage(batch.keys); err != nil {
				return err
			}
		}
		if advanced || len(batch.issues) > 0 {
			a.mu.Lock()
			a.status.LastCopyMS = time.Now().UnixMilli()
			a.mu.Unlock()
		}
		if batch.keys < forecastLongIssuePage {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

type forecastLongIssueBatch struct {
	issues []ForecastArchiveIssue
	last   forecastLongIssueCursor
	keys   int
}

// readIssuePage lists one page of keys and loads only the runs the archive
// does not hold. Model states are read outside the page deadline, as
// before, so a slow blob read does not use it up.
func (a *ForecastLongArchive) readIssuePage(ctx context.Context, src *Store, from forecastLongIssueCursor, until int64) (forecastLongIssueBatch, error) {
	var batch forecastLongIssueBatch
	pageCtx, cancel := context.WithTimeout(ctx, forecastLongPageTimeout)
	defer cancel()
	keys, err := src.forecastIssueKeysAfter(pageCtx, from, until, forecastLongIssuePage)
	if err != nil || len(keys) == 0 {
		return batch, fromForecastSource(err)
	}
	for _, key := range keys {
		known, err := a.hasRun(pageCtx, key.ID)
		if err != nil {
			return batch, err
		}
		if known {
			continue
		}
		item, err := src.forecastIssueCopy(pageCtx, key)
		if errors.Is(err, sql.ErrNoRows) {
			continue // pruned from state.db after the key list
		}
		if err != nil {
			return batch, fromForecastSource(err)
		}
		if item.Err != nil {
			a.warn("decode", item.Err)
			continue
		}
		states, missing, err := forecastLongStatesFrom(ctx, src, item.Issue)
		if err != nil {
			return batch, err
		}
		if missing > 0 {
			// state.db pruned these blobs first; the run keeps its ids.
			slog.Info("forecast long archive: model state no longer in state.db", "run", key.ID, "missing", missing)
		}
		batch.issues = append(batch.issues, ForecastArchiveIssue{
			Issue: item.Issue, JSONBytes: item.JSONBytes, GzipBytes: item.GzipBytes, States: states,
		})
	}
	batch.last, batch.keys = keys[len(keys)-1], len(keys)
	return batch, nil
}

func (a *ForecastLongArchive) hasRun(ctx context.Context, id string) (bool, error) {
	var one int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM fa_runs WHERE run_id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func forecastLongStatesFrom(ctx context.Context, src *Store, issue forecasting.Issue) (map[string]ForecastModelBlob, int, error) {
	states := map[string]ForecastModelBlob{}
	missing := 0
	for _, model := range issue.Models {
		if model.StateID == "" {
			continue
		}
		if _, ok := states[model.StateID]; ok {
			continue
		}
		expanded, payload, err := src.LoadForecastModelStateRaw(ctx, model.StateID)
		if errors.Is(err, sql.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			return nil, 0, fromForecastSource(err)
		}
		states[model.StateID] = ForecastModelBlob{Expanded: expanded, Gzip: payload}
	}
	return states, missing, nil
}

// copyObservations follows the same rules as copyIssues on the
// available_at_ms order. Rows read again are ignored on insert.
func (a *ForecastLongArchive) copyObservations(ctx context.Context, src *Store, now time.Time) error {
	until := now.UnixMilli() + maxForecastFutureSkew.Milliseconds()
	cursor, err := a.obsCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.AvailableMS > until {
		slog.Warn("forecast long archive: observation cursor is ahead of the clock; copying the 30-day table again",
			"cursor_ms", cursor.AvailableMS, "now_ms", now.UnixMilli())
		cursor = forecastLongObsCursor{}
		if err := a.writeObsCursor(ctx, cursor); err != nil {
			return err
		}
	}
	from := forecastLongObsCursor{AvailableMS: cursor.AvailableMS - forecastLongCopyOverlap.Milliseconds()}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageCtx, cancel := context.WithTimeout(ctx, forecastLongPageTimeout)
		page, err := src.forecastObservationsAfter(pageCtx, from, until, forecastLongObsPage)
		cancel()
		if err != nil {
			return fromForecastSource(err)
		}
		if len(page) == 0 {
			return nil
		}
		from = page[len(page)-1].Key
		rows := make([]forecasting.Observation, 0, len(page))
		for _, row := range page {
			if row.Err != nil {
				a.warn("decode", row.Err)
				continue
			}
			rows = append(rows, row.Observation)
		}
		writeCtx, cancelWrite := context.WithTimeout(ctx, forecastLongPageTimeout)
		err = a.ArchiveObservations(writeCtx, rows)
		if err == nil && from.after(cursor) {
			cursor = from
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
