package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastIssueWriterContentionAllowsIdenticalRetry(t *testing.T) {
	store := openForecastArchive(t)
	store.db.SetMaxOpenConns(2)
	store.db.SetMaxIdleConns(2)
	first, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.db.Conn(context.Background())
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	for _, conn := range []*sql.Conn{first, second} {
		if _, err = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=50"); err != nil {
			first.Close()
			second.Close()
			t.Fatal(err)
		}
	}
	if err = first.Close(); err != nil {
		second.Close()
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	issue := archiveIssue("contended", now-2*int64(time.Hour/time.Millisecond), now)
	state := append([]byte(`{"weights":"`), bytes.Repeat([]byte("x"), 512<<10)...)
	state = append(state, []byte(`"}`)...)
	issue.Models = []forecasting.ModelState{{
		Name: "load", Version: "v1", Quality: forecasting.ModelQualityWarm,
		UpdatedAtMS: issue.OriginMS, State: state,
	}}

	// Hold SQLite's writer lock on a separate connection. SaveForecastIssue
	// can prepare the full archive row, but its first write must wait.
	blocker, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	blocked := true
	defer func() {
		if blocked {
			blocker.Rollback()
		}
	}()
	if _, err = blocker.Exec(`INSERT INTO forecast_observations
 (start_ms,end_ms,available_at_ms,config_version,payload) VALUES(-2,-1,-1,'lock','{}')`); err != nil {
		t.Fatal(err)
	}

	blockedCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = store.SaveForecastIssue(blockedCtx, issue)
	cancel()
	if err == nil {
		t.Fatal("contended save succeeded while another transaction held SQLite's writer lock")
	}
	var sqliteErr interface{ Code() int }
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("contended save error=%v, want wrapped SQLite error", err)
	}
	if code := sqliteErr.Code() & 0xff; code != 5 && code != 6 {
		t.Fatalf("contended save SQLite code=%d error=%v, want BUSY or LOCKED", sqliteErr.Code(), err)
	}
	var count int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM forecast_issues WHERE id=?", issue.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("timed-out save committed %d forecast issues, want 0", count)
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	blocked = false

	retryCtx, retryCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer retryCancel()
	if err = store.SaveForecastIssue(retryCtx, issue); err != nil {
		t.Fatalf("retry after writer contention: %v", err)
	}
	if err = store.SaveForecastIssue(retryCtx, issue); err != nil {
		t.Fatalf("identical retry after successful save: %v", err)
	}

	var refs, states int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM forecast_issues WHERE id=?", issue.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err = store.db.QueryRow("SELECT COUNT(*) FROM forecast_issue_model_states WHERE issue_id=?", issue.ID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if err = store.db.QueryRow("SELECT COUNT(*) FROM forecast_model_states").Scan(&states); err != nil {
		t.Fatal(err)
	}
	if count != 1 || refs != 1 || states != 1 {
		t.Fatalf("archive rows after retries: issues=%d refs=%d states=%d, want 1 each", count, refs, states)
	}
	issues, err := store.LoadForecastIssues(context.Background(), 0, time.Now().Add(time.Hour).UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || len(issues[0].Models) != 1 || issues[0].Models[0].StateID == "" {
		t.Fatalf("stored issue does not contain one model-state reference: %+v", issues)
	}
	loaded, err := store.LoadForecastModelState(context.Background(), issues[0].Models[0].StateID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, state) {
		t.Fatal("model state changed across contention and retry")
	}
}
