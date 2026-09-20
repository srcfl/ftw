package state

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// A few old minutes must not scan every newer dashboard row once per bucket.
// This is the shape of the Pi's first aggregate-retention run after an update.
func TestDashboardRollupWithLargeRecentWindow(t *testing.T) {
	s := freshStore(t)
	const rows = 20000
	const buckets = 128
	const width = int64(60000)
	base := time.Now().UTC().Truncate(time.Hour).UnixMilli()
	tx, err := s.history.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO history_dashboard VALUES(?,10000,?,?,3,100,0,0,100,0.5,?,X'')`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	detail := strings.Repeat("x", 4096)
	for i := 0; i < rows; i++ {
		ms := base + int64(i)*10000
		if _, err := stmt.Exec(ms, ms, ms+9000, fmt.Sprintf(`{"sample":%d,"detail":"%s"}`, i, detail)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if err := s.rollDashboard(ctx, 10000, width, base+buckets*width); err != nil {
		t.Fatal(err)
	}
	t.Logf("rolled %d source rows beside %d recent rows in %s", buckets*6, rows-buckets*6, time.Since(started))
	var count, samples int64
	if err := s.history.QueryRow(`SELECT COUNT(*),SUM(n) FROM history_dashboard WHERE resolution_ms=60000`).Scan(&count, &samples); err != nil {
		t.Fatal(err)
	}
	if count != buckets || samples != buckets*6*3 {
		t.Fatalf("buckets=%d samples=%d", count, samples)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_dashboard WHERE resolution_ms=10000`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != rows-buckets*6 {
		t.Fatalf("recent rows=%d", count)
	}
	var newest string
	if err := s.history.QueryRow(`SELECT json FROM history_dashboard WHERE start_ms=? AND resolution_ms=60000`, base).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newest, `"sample":5,`) {
		t.Fatal("lost newest detail")
	}
}

func TestAggregateDashboardReadYieldsAndRetriesLateRows(t *testing.T) {
	reading, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	fn := fmt.Sprintf("test_dashboard_read_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		entered.Do(func() { close(reading); <-release })
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	s := freshStore(t)
	for _, q := range []string{
		`ALTER TABLE history_dashboard RENAME TO dashboard_source`,
		`INSERT INTO dashboard_source VALUES(0,10000,0,9000,3,100,0,0,100,0.5,'first',X'')`,
		`CREATE VIEW history_dashboard AS SELECT start_ms,resolution_ms,first_ms,last_ms,n,grid_w,pv_w,bat_w,load_w,bat_soc,` + fn + `(json) AS json,seen_ms FROM dashboard_source`,
		`CREATE TRIGGER insert_dashboard INSTEAD OF INSERT ON history_dashboard BEGIN
 INSERT OR REPLACE INTO dashboard_source VALUES(NEW.start_ms,NEW.resolution_ms,NEW.first_ms,NEW.last_ms,NEW.n,NEW.grid_w,NEW.pv_w,NEW.bat_w,NEW.load_w,NEW.bat_soc,NEW.json,NEW.seen_ms); END`,
		`CREATE TRIGGER delete_dashboard INSTEAD OF DELETE ON history_dashboard BEGIN
 DELETE FROM dashboard_source WHERE start_ms=OLD.start_ms AND resolution_ms=OLD.resolution_ms; END`,
	} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.rollDashboard(ctx, 10000, 60000, 60000) }()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := s.EnqueueTelemetryTick(&HistoryPoint{TsMs: time.Now().UnixMilli(), GridW: 42, JSON: "{}"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	flush, stop := context.WithTimeout(ctx, time.Second)
	err := s.FlushHistory(flush)
	stop()
	if err != nil {
		released.Do(func() { close(release) })
		<-done
		t.Fatal("dashboard scan blocked live writes:", err)
	}
	if _, err := s.history.Exec(`INSERT INTO dashboard_source VALUES(10000,10000,10000,19000,1,300,0,0,300,0.9,'last',X'')`); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var n, first, last int64
	var power, soc float64
	var detail string
	if err := s.history.QueryRow(`SELECT n,first_ms,last_ms,grid_w,bat_soc,json FROM dashboard_source WHERE resolution_ms=60000`).Scan(&n, &first, &last, &power, &soc, &detail); err != nil {
		t.Fatal(err)
	}
	if n != 4 || first != 0 || last != 19000 || power != 150 || soc != .6 || detail != "last" {
		t.Fatalf("lost or repeated late evidence: %d %d %d %v %v %s", n, first, last, power, soc, detail)
	}
}

func TestDashboardRollupFailureRetainsSourceAndRetryIsExact(t *testing.T) {
	s := freshStore(t)
	for _, q := range []string{
		`INSERT INTO history_dashboard VALUES(0,10000,0,9000,3,100,0,0,100,0.5,'last',X'')`,
		`CREATE TRIGGER fail_dashboard_delete BEFORE DELETE ON history_dashboard BEGIN SELECT RAISE(ABORT,'prune failed'); END`,
	} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.rollDashboard(ctx, 10000, 60000, 60000); err == nil || !strings.Contains(err.Error(), "prune failed") {
		t.Fatalf("expected prune error: %v", err)
	}
	var rows, n, width int64
	if err := s.history.QueryRow(`SELECT COUNT(*),SUM(n),MAX(resolution_ms) FROM history_dashboard`).Scan(&rows, &n, &width); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || n != 3 || width != 10000 {
		t.Fatalf("failed transaction changed source: %d %d %d", rows, n, width)
	}
	if _, err := s.history.Exec(`DROP TRIGGER fail_dashboard_delete`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.rollDashboard(ctx, 10000, 60000, 60000); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.history.QueryRow(`SELECT COUNT(*),SUM(n),MAX(resolution_ms) FROM history_dashboard`).Scan(&rows, &n, &width); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || n != 3 || width != 60000 {
		t.Fatalf("retry changed totals: %d %d %d", rows, n, width)
	}
}
