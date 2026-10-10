package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastScoreCommitSurvivesFailedRetention(t *testing.T) {
	s := openForecastArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-ForecastIssueRetention - 2*time.Hour).UnixMilli()
	score := archiveError("expired", start-int64(time.Hour/time.Millisecond), start-int64(time.Hour/time.Millisecond), start)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_score_prune BEFORE DELETE ON forecast_errors BEGIN SELECT RAISE(ABORT,'prune failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveForecastErrors(ctx, []forecasting.ErrorSample{score}, now.UnixMilli()); err != nil {
		t.Fatalf("a retention failure masked a committed page: %v", err)
	}
	if err := s.PruneForecastErrors(ctx, now.UnixMilli()); err == nil || !strings.Contains(err.Error(), "prune failed") {
		t.Fatalf("want independent maintenance failure, got %v", err)
	}
	got, err := s.LoadForecastErrors(ctx, start, now.UnixMilli(), false)
	if err != nil || len(got) != 1 || got[0].IssueID != score.IssueID {
		t.Fatalf("commit lost: %+v %v", got, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_score_prune`); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneForecastErrors(ctx, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadForecastErrors(ctx, start, now.UnixMilli(), false)
	if err != nil || len(got) != 0 {
		t.Fatalf("maintenance retry did not expire scores: %+v %v", got, err)
	}
}

func TestForecastRetentionUsesMetadataIndexAfterUpgrade(t *testing.T) {
	s := openForecastArchive(t)
	ctx := context.Background()
	// Reopen an archive from before the retention index existed.
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS forecast_errors_retention`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-2 * time.Hour).UnixMilli()
	score := archiveError("retained", start-int64(time.Hour/time.Millisecond), start-int64(time.Hour/time.Millisecond), start)
	if err := s.SaveForecastErrors(ctx, []forecasting.ErrorSample{score}, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.InitForecastArchive(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for name, query := range map[string]string{
		"budget": `SELECT COUNT(*),COALESCE(SUM(length(payload)),0),COALESCE(MIN(end_ms),0) FROM forecast_errors`,
		"eviction": `SELECT rowid,end_ms,length(payload) FROM forecast_errors
 ORDER BY end_ms DESC,start_ms DESC,series DESC,lead DESC,config_version DESC`,
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "; ")
			if !strings.Contains(joined, "USING COVERING INDEX forecast_errors_retention") || strings.Contains(joined, "TEMP B-TREE") {
				t.Fatalf("retention reads or sorts score payloads: %s", joined)
			}
		})
	}
	got, err := s.LoadForecastErrors(ctx, start, now.UnixMilli(), false)
	if err != nil || len(got) != 1 || got[0].IssueID != score.IssueID {
		t.Fatalf("upgrade lost existing score: %+v %v", got, err)
	}
}

func TestForecastRetentionLimitsAndTies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rows, bytes int
		expired     bool
	}{
		{"within", 150, 300, false}, {"rows", 55, 1000, false},
		{"bytes", 200, 109, false}, {"expiry", 200, 1000, true},
		{"combined", 55, 109, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openForecastArchive(t)
			ctx := context.Background()
			if _, err := s.db.Exec(`CREATE TABLE prune_test(id INTEGER PRIMARY KEY, expires INTEGER, payload TEXT)`); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixMilli()
			cutoff := now - ForecastIssueRetention.Milliseconds()
			for i := 1; i <= 150; i++ {
				expires := cutoff + int64(i/3)
				if tc.expired && i <= 100 {
					expires = cutoff - 1
				}
				if _, err := s.db.Exec(`INSERT INTO prune_test VALUES(?,?,?)`, i, expires, strings.Repeat("x", i%3+1)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.pruneForecastRecords(ctx, "prune_test", "expires DESC,id DESC", "expires", now, tc.rows, tc.bytes); err != nil {
				t.Fatal(err)
			}
			rows, err := s.db.Query(`SELECT id FROM prune_test ORDER BY expires DESC,id DESC`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got, want []int
			for rows.Next() {
				var id int
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			bytes := 0
			for i := 150; i >= 1; i-- {
				bytes += i%3 + 1
				if 151-i <= tc.rows && bytes <= tc.bytes && !(tc.expired && i <= 100) {
					want = append(want, i)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("retained IDs=%v want %v", got, want)
			}
		})
	}
}

func TestForecastRetentionResumesAfterCommittedDeleteBatch(t *testing.T) {
	s := openForecastArchive(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`CREATE TABLE prune_test(id INTEGER PRIMARY KEY, expires INTEGER, payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 150; i++ {
		if _, err := s.db.Exec(`INSERT INTO prune_test VALUES(?,?,'x')`, i, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER interrupt_prune BEFORE DELETE ON prune_test
 WHEN (SELECT COUNT(*) FROM prune_test)<=86 BEGIN SELECT RAISE(ABORT,'second batch blocked'); END`); err != nil {
		t.Fatal(err)
	}
	prune := func() error {
		return s.pruneForecastRecords(ctx, "prune_test", "expires DESC,id DESC", "expires", now, 50, 1000)
	}
	if err := prune(); err == nil || !strings.Contains(err.Error(), "second batch blocked") {
		t.Fatalf("expected interrupted retention, got %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM prune_test`).Scan(&count); err != nil || count != 86 {
		t.Fatalf("first delete batch lost: count=%d err=%v", count, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER interrupt_prune`); err != nil {
		t.Fatal(err)
	}
	if err := prune(); err != nil {
		t.Fatal(err)
	}
	var oldest int
	if err := s.db.QueryRow(`SELECT COUNT(*),MIN(id) FROM prune_test`).Scan(&count, &oldest); err != nil || count != 50 || oldest != 101 {
		t.Fatalf("retry lost newest records: count=%d oldest=%d err=%v", count, oldest, err)
	}
}
