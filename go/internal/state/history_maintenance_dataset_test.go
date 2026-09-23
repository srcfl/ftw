package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHistoryMaintenanceReadOnlyDataset(t *testing.T) {
	p := os.Getenv("FTW_MAINTENANCE_READONLY_DB")
	if p == "" {
		t.Skip("set FTW_MAINTENANCE_READONLY_DB")
	}
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cutoff := time.Now().Add(-AggregateRetention).UnixMilli()
	for _, query := range []struct {
		sql    string
		cutoff int64
	}{
		{`SELECT start_ms,resolution_ms FROM history_dashboard WHERE last_ms<? ORDER BY start_ms LIMIT 128`, cutoff},
		{`SELECT start_ms,resolution_ms FROM history_dashboard WHERE last_ms<? ORDER BY last_ms LIMIT 128`, cutoff},
		{`SELECT MIN(start_ms) FROM ts_buckets WHERE resolution_ms=60000 AND start_ms<?`, time.Now().Add(-AggregateRecentRetention).UnixMilli()},
		{`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms=60000 AND start_ms<?`, time.Now().Add(-AggregateRecentRetention).UnixMilli()},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		started := time.Now()
		rows, err := db.QueryContext(ctx, query.sql, query.cutoff)
		var values [][]any
		if err == nil {
			cols, _ := rows.Columns()
			for rows.Next() {
				vs := make([]any, len(cols))
				dest := make([]any, len(cols))
				for i := range vs {
					dest[i] = &vs[i]
				}
				if err = rows.Scan(dest...); err != nil {
					break
				}
				values = append(values, vs)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		t.Logf("query=%s elapsed=%s rows=%v err=%v", query.sql, time.Since(started), values, err)
		cancel()
	}
}

// Only use a disposable restored copy, never the running box's directory.
func TestHistoryMaintenanceDataset(t *testing.T) {
	dir := os.Getenv("FTW_MAINTENANCE_TEST_DATA")
	if dir == "" {
		t.Skip("set FTW_MAINTENANCE_TEST_DATA to a disposable copy")
	}
	db, err := openDurableHistory(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{history: db, coldDir: filepath.Join(dir, "cold"), historyWriter: &historyWriter{}}
	s.aggregateHistory.Store(true)
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ctx := context.WithValue(parent, archiveTurnKey{}, time.Now().Add(30*time.Second))
	defer cancel()
	stage := os.Getenv("FTW_MAINTENANCE_TEST_STAGE")
	started := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				t.Logf("progress=%+v", s.HistoryMaintenanceStatus())
			}
		}
	}()
	defer wg.Wait()
	defer cancel()
	switch stage {
	case "dashboard":
		err = s.maintainDashboard(ctx, time.Now())
	case "aggregate":
		err = s.MaintainAggregateHistory(ctx, s.coldDir, time.Now())
	case "samples":
		_, _, err = s.rolloffSamples(ctx, s.coldDir, AggregateRecentRetention)
	default:
		t.Fatal("choose dashboard, aggregate or samples")
	}
	t.Logf("stage=%s elapsed=%s err=%v", stage, time.Since(started), err)
	if errors.Is(err, errArchiveTurnComplete) {
		if s.HistoryMaintenanceStatus().RowsDone <= 0 {
			t.Fatal("yield without saved progress")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}
