package state

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

const (
	ForecastIssueRetention = 30 * 24 * time.Hour

	MaxForecastIssues           = 4096
	MaxForecastArchiveBytes     = 64 << 20
	MaxForecastModelStateBytes  = 64 << 20
	MaxForecastObservations     = 8192
	MaxForecastObservationBytes = 16 << 20
	MaxForecastErrors           = 65536
	MaxForecastErrorBytes       = 64 << 20

	maxForecastCompressedBytes = 256 << 10
	maxForecastModelCompressed = 256 << 10
	maxForecastRecordBytes     = 8 << 10
	maxForecastSliceBytes      = 32 << 20
	maxForecastFutureSkew      = 5 * time.Minute
)

// InitForecastArchive creates additive tables in precious state.db. Issued
// forecasts cannot be fetched again from a weather API. Old binaries can
// reopen the database and ignore these tables.
func (s *Store) InitForecastArchive(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS forecast_issues (
 id TEXT PRIMARY KEY, origin_ms INTEGER NOT NULL, issued_at_ms INTEGER NOT NULL,
 config_version TEXT NOT NULL, payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS forecast_issues_time ON forecast_issues(issued_at_ms);
CREATE TABLE IF NOT EXISTS forecast_model_states (
 id TEXT PRIMARY KEY, expanded_bytes INTEGER NOT NULL, created_at_ms INTEGER NOT NULL,
 payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS forecast_issue_model_states (
 issue_id TEXT NOT NULL, state_id TEXT NOT NULL,
 PRIMARY KEY(issue_id,state_id));
CREATE INDEX IF NOT EXISTS forecast_issue_model_state_ref ON forecast_issue_model_states(state_id);
CREATE TABLE IF NOT EXISTS forecast_observations (
 start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, available_at_ms INTEGER NOT NULL,
 config_version TEXT NOT NULL, payload TEXT NOT NULL,
 PRIMARY KEY(start_ms,end_ms,config_version));
CREATE INDEX IF NOT EXISTS forecast_observations_time ON forecast_observations(available_at_ms);
CREATE TABLE IF NOT EXISTS forecast_errors (
	series TEXT NOT NULL, config_version TEXT NOT NULL, lead INTEGER NOT NULL,
	start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, origin_ms INTEGER NOT NULL,
	issued_at_ms INTEGER NOT NULL, issue_id TEXT NOT NULL,
	available_at_ms INTEGER NOT NULL, payload TEXT NOT NULL,
 PRIMARY KEY(series,config_version,lead,start_ms,end_ms));
CREATE INDEX IF NOT EXISTS forecast_errors_time ON forecast_errors(start_ms);
`)
	return err
}

func gzipForecastIssue(issue forecasting.Issue) ([]byte, error) {
	data, err := json.Marshal(issue)
	if err != nil {
		return nil, err
	}
	if len(data) > forecasting.MaxPayloadBytes {
		return nil, errors.New("forecast issue exceeds payload limit")
	}
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	if _, err = z.Write(data); err != nil {
		return nil, err
	}
	if err = z.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxForecastCompressedBytes {
		return nil, errors.New("compressed forecast issue exceeds 256 KiB")
	}
	return buf.Bytes(), nil
}

func gzipForecastModelState(state json.RawMessage) ([]byte, error) {
	if len(state) == 0 || len(state) > forecasting.MaxModelStateBytes || !json.Valid(state) {
		return nil, errors.New("invalid forecast model state")
	}
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	if _, err := z.Write(state); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxForecastModelCompressed {
		return nil, errors.New("compressed forecast model state exceeds 256 KiB")
	}
	return buf.Bytes(), nil
}

type preparedForecastModelState struct {
	id            string
	expandedBytes int
	payload       []byte
	inline        bool
}

type preparedForecastIssue struct {
	issue       forecasting.Issue
	payload     []byte
	modelStates []preparedForecastModelState
}

func prepareForecastIssue(issue forecasting.Issue) (preparedForecastIssue, error) {
	issue.Models = append([]forecasting.ModelState(nil), issue.Models...)
	var prepared preparedForecastIssue
	for i := range issue.Models {
		model := &issue.Models[i]
		model.State = append(json.RawMessage(nil), model.State...)
		if len(model.State) == 0 {
			if model.StateID != "" {
				prepared.modelStates = append(prepared.modelStates, preparedForecastModelState{id: model.StateID})
			}
			continue
		}
		digest := sha256.Sum256(model.State)
		stateID := hex.EncodeToString(digest[:])
		if model.StateID != "" && model.StateID != stateID {
			return preparedForecastIssue{}, errors.New("forecast model state hash mismatch")
		}
		compressed, err := gzipForecastModelState(model.State)
		if err != nil {
			return preparedForecastIssue{}, err
		}
		prepared.modelStates = append(prepared.modelStates, preparedForecastModelState{
			id: stateID, expandedBytes: len(model.State), payload: compressed, inline: true,
		})
		model.StateID = stateID
		model.State = nil
	}
	if err := issue.Validate(); err != nil {
		return preparedForecastIssue{}, err
	}
	payload, err := gzipForecastIssue(issue)
	if err != nil {
		return preparedForecastIssue{}, err
	}
	prepared.issue = issue
	prepared.payload = payload
	return prepared, nil
}

func storePreparedForecastModelStates(ctx context.Context, tx *sql.Tx, prepared preparedForecastIssue) error {
	for _, state := range prepared.modelStates {
		if !state.inline {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM forecast_model_states WHERE id=?", state.id).Scan(&exists); err != nil {
				return err
			}
			if exists != 1 {
				return errors.New("forecast model state reference is missing")
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO forecast_model_states(id,expanded_bytes,created_at_ms,payload)
 VALUES(?,?,?,?)`, state.id, state.expandedBytes, prepared.issue.IssuedAtMS, state.payload)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var expanded int
			var old []byte
			if err = tx.QueryRowContext(ctx, "SELECT expanded_bytes,payload FROM forecast_model_states WHERE id=?", state.id).Scan(&expanded, &old); err != nil {
				return err
			}
			if expanded != state.expandedBytes || !bytes.Equal(old, state.payload) {
				return errors.New("forecast model state ID is immutable")
			}
		}
	}
	return nil
}

func cleanForecastModelStateRefs(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM forecast_issue_model_states
 WHERE issue_id NOT IN (SELECT id FROM forecast_issues)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM forecast_model_states
 WHERE id NOT IN (SELECT state_id FROM forecast_issue_model_states)`)
	return err
}

func enforceForecastModelStateBudget(ctx context.Context, tx *sql.Tx) error {
	for {
		var total int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(payload)),0) FROM forecast_model_states").Scan(&total); err != nil {
			return err
		}
		if total <= MaxForecastModelStateBytes {
			return nil
		}
		var oldest string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM forecast_issues ORDER BY issued_at_ms,id LIMIT 1").Scan(&oldest); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM forecast_issues WHERE id=?", oldest); err != nil {
			return err
		}
		if err := cleanForecastModelStateRefs(ctx, tx); err != nil {
			return err
		}
	}
}

// SaveForecastIssue is append-only within a bounded retention window.
// An identical retry is allowed; reusing an ID for a changed issue is an error.
func (s *Store) SaveForecastIssue(ctx context.Context, issue forecasting.Issue) error {
	if err := issue.Validate(); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if issue.IssuedAtMS > now+maxForecastFutureSkew.Milliseconds() {
		return errors.New("forecast issue is from the future")
	}
	prepared, err := prepareForecastIssue(issue)
	if err != nil {
		return fmt.Errorf("prepare forecast issue: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin forecast issue transaction: %w", err)
	}
	defer tx.Rollback()
	if err = storePreparedForecastModelStates(ctx, tx, prepared); err != nil {
		return fmt.Errorf("store forecast model states: %w", err)
	}
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO forecast_issues(id,origin_ms,issued_at_ms,config_version,payload) VALUES(?,?,?,?,?)",
		prepared.issue.ID, prepared.issue.OriginMS, prepared.issue.IssuedAtMS, prepared.issue.ConfigVersion, prepared.payload)
	if err != nil {
		return fmt.Errorf("store forecast issue: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect forecast issue insert: %w", err)
	}
	if n == 0 {
		var old []byte
		if err = tx.QueryRowContext(ctx, "SELECT payload FROM forecast_issues WHERE id=?", prepared.issue.ID).Scan(&old); err != nil {
			return fmt.Errorf("read existing forecast issue: %w", err)
		}
		if !bytes.Equal(old, prepared.payload) {
			return errors.New("forecast issue ID is immutable")
		}
	}
	for _, model := range prepared.issue.Models {
		if model.StateID == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO forecast_issue_model_states(issue_id,state_id) VALUES(?,?)", prepared.issue.ID, model.StateID); err != nil {
			return fmt.Errorf("store forecast model state reference: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM forecast_issues WHERE issued_at_ms < ?", now-ForecastIssueRetention.Milliseconds()); err != nil {
		return fmt.Errorf("prune expired forecast issues: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM forecast_issues WHERE id IN (
 SELECT id FROM (SELECT id,ROW_NUMBER() OVER (ORDER BY issued_at_ms DESC,id DESC) AS n,
 SUM(length(payload)) OVER (ORDER BY issued_at_ms DESC,id DESC) AS bytes FROM forecast_issues)
 WHERE n>? OR bytes>?)`, MaxForecastIssues, MaxForecastArchiveBytes); err != nil {
		return fmt.Errorf("prune forecast issue budget: %w", err)
	}
	if err = cleanForecastModelStateRefs(ctx, tx); err != nil {
		return fmt.Errorf("clean forecast model states: %w", err)
	}
	if err = enforceForecastModelStateBudget(ctx, tx); err != nil {
		return fmt.Errorf("enforce forecast model state budget: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit forecast issue: %w", err)
	}
	return nil
}

func (s *Store) SaveForecastObservation(ctx context.Context, observation forecasting.Observation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if observation.AvailableAtMS > now+maxForecastFutureSkew.Milliseconds() {
		return errors.New("forecast observation is from the future")
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if len(data) > maxForecastRecordBytes {
		return errors.New("forecast observation exceeds payload limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO forecast_observations(start_ms,end_ms,available_at_ms,config_version,payload) VALUES(?,?,?,?,?)",
		observation.StartMS, observation.EndMS, observation.AvailableAtMS, observation.ConfigVersion, string(data))
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var old string
		if err = tx.QueryRowContext(ctx, "SELECT payload FROM forecast_observations WHERE start_ms=? AND end_ms=? AND config_version=?",
			observation.StartMS, observation.EndMS, observation.ConfigVersion).Scan(&old); err != nil {
			return err
		}
		if old != string(data) {
			return errors.New("forecast observation is immutable")
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM forecast_observations WHERE available_at_ms < ?", now-ForecastIssueRetention.Milliseconds()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM forecast_observations WHERE rowid IN (
 SELECT rowid FROM (SELECT rowid,ROW_NUMBER() OVER (ORDER BY available_at_ms DESC,start_ms DESC,end_ms DESC,config_version DESC) AS n,
 SUM(length(payload)) OVER (ORDER BY available_at_ms DESC,start_ms DESC,end_ms DESC,config_version DESC) AS bytes FROM forecast_observations)
 WHERE n>? OR bytes>?)`, MaxForecastObservations, MaxForecastObservationBytes); err != nil {
		return err
	}
	return tx.Commit()
}

func decodeForecastIssue(data []byte) (forecasting.Issue, int, error) {
	if len(data) > maxForecastCompressedBytes {
		return forecasting.Issue{}, 0, errors.New("oversized forecast archive row")
	}
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return forecasting.Issue{}, 0, err
	}
	plain, readErr := io.ReadAll(io.LimitReader(z, forecasting.MaxPayloadBytes+1))
	closeErr := z.Close()
	if readErr != nil {
		return forecasting.Issue{}, 0, readErr
	}
	if closeErr != nil {
		return forecasting.Issue{}, 0, closeErr
	}
	if len(plain) > forecasting.MaxPayloadBytes {
		return forecasting.Issue{}, 0, errors.New("oversized expanded forecast")
	}
	var issue forecasting.Issue
	if err = json.Unmarshal(plain, &issue); err != nil {
		return forecasting.Issue{}, 0, err
	}
	if err = issue.Validate(); err != nil {
		return forecasting.Issue{}, 0, fmt.Errorf("invalid archived issue: %w", err)
	}
	return issue, len(plain), nil
}

// LoadForecastModelState expands and verifies one content-addressed model
// snapshot for backup or audit export. Normal scoring uses the StateID only.
func (s *Store) LoadForecastModelState(ctx context.Context, stateID string) (json.RawMessage, error) {
	decodedID, err := hex.DecodeString(stateID)
	if err != nil || len(decodedID) != sha256.Size {
		return nil, errors.New("invalid forecast model state ID")
	}
	var expanded int
	var data []byte
	if err = s.db.QueryRowContext(ctx, "SELECT expanded_bytes,payload FROM forecast_model_states WHERE id=?", stateID).Scan(&expanded, &data); err != nil {
		return nil, err
	}
	if expanded <= 0 || expanded > forecasting.MaxModelStateBytes || len(data) > maxForecastModelCompressed {
		return nil, errors.New("oversized forecast model state")
	}
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	plain, readErr := io.ReadAll(io.LimitReader(z, int64(forecasting.MaxModelStateBytes)+1))
	closeErr := z.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(plain) != expanded || len(plain) > forecasting.MaxModelStateBytes || !json.Valid(plain) {
		return nil, errors.New("invalid expanded forecast model state")
	}
	digest := sha256.Sum256(plain)
	if hex.EncodeToString(digest[:]) != stateID {
		return nil, errors.New("forecast model state hash mismatch")
	}
	return json.RawMessage(plain), nil
}

// LoadForecastIssues loads a bounded slice for tests and small tools. Runtime
// evaluation should use VisitForecastIssues so expanded issues do not collect.
func (s *Store) LoadForecastIssues(ctx context.Context, since, until int64, limit int) ([]forecasting.Issue, error) {
	if limit <= 0 || limit > MaxForecastIssues {
		limit = MaxForecastIssues
	}
	rows, err := s.db.QueryContext(ctx, "SELECT payload FROM forecast_issues WHERE issued_at_ms>=? AND issued_at_ms<=? ORDER BY issued_at_ms DESC,id DESC LIMIT ?",
		since, until, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]forecasting.Issue, 0, min(limit, 64))
	expandedBytes := 0
	for rows.Next() {
		if len(out) == limit {
			return nil, fmt.Errorf("forecast issue load exceeds slice limit %d; use VisitForecastIssues", limit)
		}
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		issue, size, decodeErr := decodeForecastIssue(data)
		if decodeErr != nil {
			return nil, decodeErr
		}
		expandedBytes += size
		if expandedBytes > maxForecastSliceBytes {
			return nil, errors.New("expanded forecast issue slice exceeds memory limit; use VisitForecastIssues")
		}
		out = append(out, issue)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) LoadForecastObservations(ctx context.Context, since, until int64) ([]forecasting.Observation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM forecast_observations
 WHERE start_ms>=? AND end_ms<=? ORDER BY start_ms,end_ms,config_version LIMIT ?`, since, until, MaxForecastObservations+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]forecasting.Observation, 0, 3000)
	totalBytes := 0
	for rows.Next() {
		if len(out) == MaxForecastObservations {
			return nil, errors.New("forecast observation load exceeds memory limit")
		}
		var data string
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		totalBytes += len(data)
		if len(data) > maxForecastRecordBytes || totalBytes > MaxForecastObservationBytes {
			return nil, errors.New("oversized forecast observation load")
		}
		var observation forecasting.Observation
		if err = json.Unmarshal([]byte(data), &observation); err != nil {
			return nil, err
		}
		if err = observation.Validate(); err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, rows.Err()
}

// VisitForecastIssues decodes one bounded record at a time. The box can score a
// month of archived forecasts without retaining a month's model snapshots.
func (s *Store) VisitForecastIssues(ctx context.Context, since, until int64, visit func(forecasting.Issue) error) error {
	if visit == nil {
		return errors.New("forecast issue visitor is nil")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT payload FROM forecast_issues WHERE issued_at_ms>=? AND issued_at_ms<=? ORDER BY issued_at_ms,id", since, until)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return err
		}
		issue, _, decodeErr := decodeForecastIssue(data)
		if decodeErr != nil {
			return decodeErr
		}
		if err = visit(issue); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SaveForecastErrors stores derived scores; immutable issue and observation
// rows retain the evidence. A newer issue for the same target and lead wins.
func (s *Store) SaveForecastErrors(ctx context.Context, samples []forecasting.ErrorSample, now int64) error {
	if now <= 0 {
		return errors.New("invalid forecast score time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sample := range samples {
		if err = sample.Validate(); err != nil {
			return err
		}
		if sample.AvailableAtMS > now || sample.EndMS > now {
			return errors.New("future forecast score")
		}
		data, marshalErr := json.Marshal(sample)
		if marshalErr != nil {
			return marshalErr
		}
		if len(data) > maxForecastRecordBytes {
			return errors.New("forecast error exceeds payload limit")
		}
		result, execErr := tx.ExecContext(ctx, `INSERT INTO forecast_errors(series,config_version,lead,start_ms,end_ms,origin_ms,issued_at_ms,issue_id,available_at_ms,payload)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(series,config_version,lead,start_ms,end_ms) DO UPDATE SET
 origin_ms=excluded.origin_ms,issued_at_ms=excluded.issued_at_ms,issue_id=excluded.issue_id,
 available_at_ms=excluded.available_at_ms,payload=excluded.payload
 WHERE excluded.origin_ms>forecast_errors.origin_ms
 OR (excluded.origin_ms=forecast_errors.origin_ms AND excluded.issued_at_ms>forecast_errors.issued_at_ms)
 OR (excluded.origin_ms=forecast_errors.origin_ms AND excluded.issued_at_ms=forecast_errors.issued_at_ms AND excluded.issue_id>forecast_errors.issue_id)`,
			sample.Series, sample.ConfigVersion, sample.Lead, sample.StartMS, sample.EndMS, sample.OriginMS,
			sample.IssuedAtMS, sample.IssueID, sample.AvailableAtMS, string(data))
		if execErr != nil {
			return execErr
		}
		if n, _ := result.RowsAffected(); n == 0 {
			var oldOrigin, oldIssued int64
			var oldIssueID string
			var old string
			if err = tx.QueryRowContext(ctx, `SELECT origin_ms,issued_at_ms,issue_id,payload FROM forecast_errors
 WHERE series=? AND config_version=? AND lead=? AND start_ms=? AND end_ms=?`, sample.Series, sample.ConfigVersion,
				sample.Lead, sample.StartMS, sample.EndMS).Scan(&oldOrigin, &oldIssued, &oldIssueID, &old); err != nil {
				return err
			}
			if oldOrigin == sample.OriginMS && oldIssued == sample.IssuedAtMS && oldIssueID == sample.IssueID && old != string(data) {
				return errors.New("forecast error for one issue origin is immutable")
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM forecast_errors WHERE end_ms < ?", now-ForecastIssueRetention.Milliseconds()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM forecast_errors WHERE rowid IN (
 SELECT rowid FROM (SELECT rowid,ROW_NUMBER() OVER (ORDER BY end_ms DESC,start_ms DESC,series DESC,lead DESC) AS n,
 SUM(length(payload)) OVER (ORDER BY end_ms DESC,start_ms DESC,series DESC,lead DESC) AS bytes FROM forecast_errors)
 WHERE n>? OR bytes>?)`, MaxForecastErrors, MaxForecastErrorBytes); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadForecastErrors reads a bounded set of valid residuals. With hourly set,
// adjacent replans and quarter-hour points cannot crowd independent days out.
func (s *Store) LoadForecastErrors(ctx context.Context, since, until int64, hourly bool) ([]forecasting.ErrorSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM forecast_errors WHERE start_ms>=? AND end_ms<=? AND available_at_ms<=?
 AND (?=0 OR start_ms%3600000=0) ORDER BY start_ms DESC,series,lead LIMIT ?`, since, until, until, hourly, MaxForecastErrors+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]forecasting.ErrorSample, 0, min(MaxForecastErrors, 40000))
	totalBytes := 0
	for rows.Next() {
		if len(out) == MaxForecastErrors {
			return nil, errors.New("forecast error load exceeds memory limit")
		}
		var data string
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		totalBytes += len(data)
		if len(data) > maxForecastRecordBytes || totalBytes > MaxForecastErrorBytes {
			return nil, errors.New("oversized forecast error load")
		}
		var sample forecasting.ErrorSample
		if err = json.Unmarshal([]byte(data), &sample); err != nil {
			return nil, err
		}
		if err = sample.Validate(); err != nil {
			return nil, fmt.Errorf("invalid archived forecast error: %w", err)
		}
		out = append(out, sample)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
