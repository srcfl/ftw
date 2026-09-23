package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
	sqlite "modernc.org/sqlite/lib"
)

func TestForecastArchiveSurvivesWriterContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}

	// Another real SQLite writer holds the lock longer than the old two-second
	// archive deadline. Reads and the planner's nonblocking queue still work.
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	at := time.Now().UTC().Truncate(time.Minute)
	f := trackerFixture(at)
	f.store = st
	slots := trackerSlots(at.Add(time.Minute), 193)
	f.Snapshot(at, trackerWeather(at.Add(time.Minute), at)).Record(slots, slots, "contended-decision", at.UnixMilli())
	frozen := <-f.queue
	f.queue <- frozen
	workerCtx, stop := context.WithCancel(ctx)
	f.cancel = stop
	f.wg.Add(1)
	go func() { defer f.wg.Done(); f.run(workerCtx) }()
	defer f.Stop()

	// Start the lock hold only after the worker has taken the issued forecast.
	waitUntil := time.NewTimer(5 * time.Second)
	defer waitUntil.Stop()
	for len(f.queue) != 0 {
		select {
		case <-waitUntil.C:
			t.Fatal("archive worker did not take the queued forecast")
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(3 * time.Second)
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	f.Snapshot(at.Add(time.Second), trackerWeather(at.Add(time.Minute), at)).Record(slots, slots, "next-decision", at.Add(time.Second).UnixMilli())

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		issues, err := st.LoadForecastIssues(ctx, at.Add(-time.Hour).UnixMilli(), at.Add(time.Hour).UnixMilli(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) == 2 {
			for _, issue := range issues {
				if issue.ID == frozen.issue.ID {
					assertFrozenForecastArchived(t, ctx, st, frozen.issue, issue)
					return
				}
			}
			t.Fatal("archive replaced the contended forecast's ID")
		}
		select {
		case <-deadline.C:
			t.Fatalf("writer contention lost an issued forecast: saved %d, want 2", len(issues))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type archiveSQLiteError int

func (e archiveSQLiteError) Error() string { return fmt.Sprintf("SQLite error %d", e) }
func (e archiveSQLiteError) Code() int     { return int(e) }

func TestForecastArchiveRetryPreservesFrozenIssue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failure    error
		commitThen bool
	}{
		{"deadline before commit", context.DeadlineExceeded, false},
		{"busy", archiveSQLiteError(sqlite.SQLITE_BUSY), false},
		{"locked shared cache", archiveSQLiteError(sqlite.SQLITE_LOCKED_SHAREDCACHE), false},
		{"ambiguous commit", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if err := st.InitForecastArchive(ctx); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Truncate(time.Minute)
			f := trackerFixture(at)
			slots := trackerSlots(at.Add(time.Minute), 193)
			f.Snapshot(at, trackerWeather(at.Add(time.Minute), at)).Record(slots, slots, "frozen-decision", at.UnixMilli())
			frozen := (<-f.queue).issue
			calls := 0
			err = saveForecastIssueWithRetry(ctx, frozen, func(writeCtx context.Context, issue forecasting.Issue) error {
				calls++
				if calls > 1 || tc.commitThen {
					if err := st.SaveForecastIssue(writeCtx, issue); err != nil {
						return err
					}
				}
				if calls == 1 {
					return fmt.Errorf("write outcome: %w", tc.failure)
				}
				return nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("archive after transient failure: calls=%d err=%v", calls, err)
			}
			issues, err := st.LoadForecastIssues(ctx, at.Add(-time.Hour).UnixMilli(), at.Add(time.Hour).UnixMilli(), 10)
			if err != nil || len(issues) != 1 {
				t.Fatalf("identical retry did not produce exactly one issue: count=%d err=%v", len(issues), err)
			}
			assertFrozenForecastArchived(t, ctx, st, frozen, issues[0])

			// A conflicting ID must remain an error, without retrying or replacing
			// the forecast whose commit succeeded on an earlier attempt.
			conflict := frozen
			conflict.DecisionID = "different-decision"
			calls = 0
			err = saveForecastIssueWithRetry(ctx, conflict, func(writeCtx context.Context, issue forecasting.Issue) error {
				calls++
				return st.SaveForecastIssue(writeCtx, issue)
			})
			if err == nil || calls != 1 {
				t.Fatalf("immutable conflict was retried or accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestForecastArchiveRetryStops(t *testing.T) {
	t.Run("bounded transient failure", func(t *testing.T) {
		calls := 0
		err := saveForecastIssueWithRetry(context.Background(), forecasting.Issue{}, func(context.Context, forecasting.Issue) error {
			calls++
			return context.DeadlineExceeded
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 3 {
			t.Fatalf("retry budget: calls=%d err=%v", calls, err)
		}
	})
	t.Run("parent already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := saveForecastIssueWithRetry(ctx, forecasting.Issue{}, func(context.Context, forecasting.Issue) error {
			t.Fatal("save called after parent cancellation")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("cancel during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		err := saveForecastIssueWithRetry(ctx, forecasting.Issue{}, func(context.Context, forecasting.Issue) error {
			calls++
			time.AfterFunc(10*time.Millisecond, cancel)
			return context.DeadlineExceeded
		})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("parent cancellation: calls=%d err=%v", calls, err)
		}
	})
	for _, permanent := range []error{context.Canceled, errors.New("invalid issue"), archiveSQLiteError(sqlite.SQLITE_FULL)} {
		calls := 0
		err := saveForecastIssueWithRetry(context.Background(), forecasting.Issue{}, func(context.Context, forecasting.Issue) error {
			calls++
			return permanent
		})
		if !errors.Is(err, permanent) || calls != 1 {
			t.Fatalf("permanent failure: calls=%d err=%v, want %v", calls, err, permanent)
		}
	}
}

func assertFrozenForecastArchived(t *testing.T, ctx context.Context, st *state.Store, frozen, stored forecasting.Issue) {
	t.Helper()
	if len(stored.Models) != len(frozen.Models) {
		t.Fatalf("stored model count = %d, want %d", len(stored.Models), len(frozen.Models))
	}
	for i := range stored.Models {
		if len(frozen.Models[i].State) == 0 {
			continue
		}
		body, err := st.LoadForecastModelState(ctx, stored.Models[i].StateID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, frozen.Models[i].State) {
			t.Fatalf("retry changed frozen model %q", stored.Models[i].Name)
		}
		stored.Models[i].State = body
		stored.Models[i].StateID = frozen.Models[i].StateID
	}
	want, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("retry changed the issued forecast, source choices, inputs or model metadata")
	}
}
