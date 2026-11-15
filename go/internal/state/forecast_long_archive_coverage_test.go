package state

import (
	"context"
	"testing"
	"time"
)

func TestForecastLongCoverageClipsToArchiveStart(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	start := now.Add(-4 * time.Hour)
	for _, hour := range []int{0, 1, 3} {
		at := start.Add(time.Duration(hour) * time.Hour).UnixMilli()
		if _, err := arch.db.ExecContext(ctx, `INSERT INTO fa_runs(
 run_id, decision_id, origin_ms, issued_at_ms, latest_input_ms, config_version, models, input_hash)
 VALUES(?, '', ?, ?, 0, 'cfg', '[]', 'hash')`, "h"+string(rune('0'+hour)), at, at); err != nil {
			t.Fatal(err)
		}
	}
	quarter := int64(15 * 60 * 1000)
	// Four known quarters and four unknown ones inside the clipped window.
	for i := 0; i < 8; i++ {
		known := 0
		if i < 4 {
			known = 1
		}
		at := start.UnixMilli() + int64(i)*quarter
		if _, err := arch.db.ExecContext(ctx, `INSERT INTO fa_observations_15m(
 start_ms, end_ms, config_version, available_at_ms, pv_w, load_w, pv_known, load_known, quality)
 VALUES(?, ?, 'cfg', ?, 1, 1, 1, ?, 'complete')`, at, at+quarter, at+quarter, known); err != nil {
			t.Fatal(err)
		}
	}
	got := arch.hourCoverage(ctx, now, 7*24*time.Hour, start.UnixMilli())
	// Hours 08:00 through 12:00 inclusive once clipped to 08:00, with 08, 09
	// and 11 present. 12:00 itself has no run.
	if got != 0.6 {
		t.Fatalf("coverage = %v, want 0.6", got)
	}
	truth := arch.truthCoverage(ctx, now, start.UnixMilli())
	span := (now.Truncate(15*time.Minute).UnixMilli() - start.UnixMilli()) / quarter
	if truth != float64(4)/float64(span) {
		t.Fatalf("truth coverage = %v, want %v", truth, float64(4)/float64(span))
	}
	at := start.Add(3 * time.Hour)
	if all := arch.hourCoverage(ctx, at, time.Hour, at.UnixMilli()); all != 1 {
		t.Fatalf("coverage clipped to a later start = %v, want 1", all)
	}
}
