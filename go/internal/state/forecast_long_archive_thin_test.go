package state

import (
	"context"
	"testing"
	"time"
)

func TestForecastLongThinningKeepsOneRunPerHour(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	young := now.Add(-24 * time.Hour).Truncate(time.Hour)
	issues := []struct {
		id     string
		origin time.Time
		issued time.Time
	}{
		{"hour-keep", old, old.Add(time.Second)},
		{"hour-later", old, old.Add(2 * time.Second)},
		{"hour-quarter", old.Add(15 * time.Minute), old},
		{"next-hour", old.Add(time.Hour), old.Add(time.Hour)},
		{"young-a", young, young},
		{"young-b", young.Add(15 * time.Minute), young},
	}
	for _, item := range issues {
		issue := longIssue(item.id, item.origin)
		issue.IssuedAtMS = item.issued.UnixMilli()
		if issue.IssuedAtMS < issue.OriginMS {
			issue.IssuedAtMS = issue.OriginMS
		}
		if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
			t.Fatalf("%s: %v", item.id, err)
		}
	}
	// The quarter-hour run is issued at the same instant as its origin above
	// when that would otherwise precede the origin. Give it a later issue time
	// that is still earlier than the :00 run's issue time only if the :00
	// preference is what keeps hour-keep.
	if err := arch.thin(ctx, now); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	rows, err := arch.db.Query(`SELECT run_id, thinned FROM fa_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	thinned := map[string]int{}
	for rows.Next() {
		var id string
		var flag int
		if err := rows.Scan(&id, &flag); err != nil {
			t.Fatal(err)
		}
		kept[id] = true
		thinned[id] = flag
	}
	if kept["hour-later"] || kept["hour-quarter"] || !kept["hour-keep"] || !kept["next-hour"] {
		t.Fatalf("old hours = %v", kept)
	}
	if !kept["young-a"] || !kept["young-b"] || thinned["young-a"] != 0 || thinned["young-b"] != 0 {
		t.Fatalf("young runs were thinned: %v flags %v", kept, thinned)
	}
	if thinned["hour-keep"] != 1 || thinned["next-hour"] != 1 {
		t.Fatalf("kept old runs not marked thinned: %v", thinned)
	}
	var states int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_run_states`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	// One reference for the first kept run of each local day. Both old hours
	// are the same local day, so one reference remains. Young runs are not thinned.
	if states != 3 {
		t.Fatalf("state references = %d, want the daily old reference plus two young runs", states)
	}
	var weather int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_weather`).Scan(&weather); err != nil || weather != 4 {
		t.Fatalf("weather rows = %d err=%v, want one per kept run", weather, err)
	}
}

func TestForecastLongDeletesAreBatched(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	var batches []int
	forecastLongAfterDeleteBatch = func(table string, n int) {
		if table == "fa_runs" {
			batches = append(batches, n)
		}
	}
	t.Cleanup(func() { forecastLongAfterDeleteBatch = nil })
	for i := 0; i < 70; i++ {
		// Same UTC hour. The smallest id is kept; the other 69 are one full delete batch plus a remainder.
		issue := longIssue("b-"+forecastLongItoa(i), old)
		if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	if err := arch.thin(ctx, now); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs after thinning = %d err=%v", runs, err)
	}
	sawFull := false
	for _, n := range batches {
		if n > forecastPruneBatch {
			t.Fatalf("delete batch %d exceeds %d", n, forecastPruneBatch)
		}
		if n == forecastPruneBatch {
			sawFull = true
		}
	}
	if !sawFull {
		t.Fatalf("batches = %v, want a full batch of %d", batches, forecastPruneBatch)
	}
}
