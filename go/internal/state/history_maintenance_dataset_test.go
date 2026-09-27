package state

import (
	"context"
	"database/sql"
	"os"
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
