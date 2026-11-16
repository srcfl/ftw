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

func TestForecastLongDailyPassesThinWholeHoursAndDays(t *testing.T) {
	for _, tc := range []struct {
		name, zone string
		from, to   time.Time
	}{
		{"stockholm", "Europe/Stockholm", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)},
		{"kolkata", "Asia/Kolkata", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			thinDaily(t, tc.zone, tc.from, tc.to)
		})
	}
}

func TestForecastLongDailyPassesThinDSTDays(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to time.Time
		day      time.Time
		hours    int
	}{
		{"spring", time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC), 23},
		{"autumn", time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arch := thinDaily(t, "Europe/Stockholm", tc.from, tc.to)
			loc, err := time.LoadLocation("Europe/Stockholm")
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(tc.day.Year(), tc.day.Month(), tc.day.Day(), 0, 0, 0, 0, loc)
			end := start.AddDate(0, 0, 1)
			var runs, holders int
			if err := arch.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT s.run_id) FROM fa_runs r
 LEFT JOIN fa_run_states s ON s.run_id=r.run_id WHERE r.origin_ms>=? AND r.origin_ms<?`,
				start.UnixMilli(), end.UnixMilli()).Scan(&runs, &holders); err != nil {
				t.Fatal(err)
			}
			if runs != tc.hours || holders != 1 {
				t.Fatalf("%s kept %d runs and %d state holders, want %d and 1", start.Format("2006-01-02"), runs, holders, tc.hours)
			}
		})
	}
}

func TestForecastLongLateRunLosesToThinnedSurvivor(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 15, 12, 2, 0, 0, time.UTC)
	hour := now.Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: longIssue("keep", hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := arch.thin(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Ranked afresh, the late run on the hour would win: it was issued
	// earlier and its id sorts first.
	late := longIssue("a-late", hour)
	late.IssuedAtMS = hour.UnixMilli()
	quarter := longIssue("late-quarter", hour.Add(30*time.Minute))
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: late}, {Issue: quarter}}); err != nil {
		t.Fatal(err)
	}
	if err := arch.thin(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"keep": true})
	var holder string
	if err := arch.db.QueryRow(`SELECT DISTINCT run_id FROM fa_run_states`).Scan(&holder); err != nil || holder != "keep" {
		t.Fatalf("state holder = %q err=%v", holder, err)
	}
	if err := arch.thin(ctx, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"keep": true})
}

// thinDaily archives a run every 15 minutes over [from, to) in zone, then
// thins once a day at 18:40 UTC until every day is past the cutoff. After
// each pass every complete UTC hour holds only its run on the hour and every
// complete local day keeps model-state references on its first run. Later
// hours and days are untouched. The pass falls 40 minutes into an hour, so
// the open hour already holds runs a pass could wrongly thin, and 10 minutes
// after a Kolkata day ends, so that day must wait for its last UTC hour.
func thinDaily(t *testing.T, zone string, from, to time.Time) *ForecastLongArchive {
	t.Helper()
	_, arch := openLongArchive(t)
	ctx := context.Background()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	var batch []ForecastArchiveIssue
	for at := from; at.Before(to); at = at.Add(15 * time.Minute) {
		issue := longIssue("q-"+at.Format("0102T1504"), at)
		issue.Site.Timezone = zone
		batch = append(batch, ForecastArchiveIssue{Issue: issue})
		if len(batch) == 96 || !at.Add(15*time.Minute).Before(to) {
			if err := arch.ArchiveIssues(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	hourMS := forecastLongHourMS
	for cutoff := from.Add(18*time.Hour + 40*time.Minute); cutoff.Before(to.Add(36 * time.Hour)); cutoff = cutoff.Add(24 * time.Hour) {
		if err := arch.thin(ctx, cutoff.Add(arch.fullDetail)); err != nil {
			t.Fatal(err)
		}
		cut := cutoff.UnixMilli() / hourMS * hourMS
		runs := map[int64][]string{}
		holders := map[string]bool{}
		rows, err := arch.db.Query(`SELECT r.run_id, r.origin_ms, EXISTS(SELECT 1 FROM fa_run_states s WHERE s.run_id=r.run_id)
 FROM fa_runs r ORDER BY r.origin_ms`)
		if err != nil {
			t.Fatal(err)
		}
		var order []int64
		ids := map[int64]string{}
		for rows.Next() {
			var id string
			var origin int64
			var refs int
			if err := rows.Scan(&id, &origin, &refs); err != nil {
				t.Fatal(err)
			}
			h := origin / hourMS * hourMS
			runs[h] = append(runs[h], id)
			order = append(order, origin)
			ids[origin] = id
			if refs != 0 {
				holders[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		for h := from.UnixMilli(); h < to.UnixMilli(); h += hourMS {
			got := runs[h]
			if h+hourMS <= cut {
				if len(got) != 1 || got[0] != ids[h] {
					t.Fatalf("pass %s: complete hour %s kept %v, want only its run on the hour",
						cutoff.Format(time.RFC3339), time.UnixMilli(h).UTC().Format(time.RFC3339), got)
				}
			} else if len(got) != 4 {
				t.Fatalf("pass %s: open hour %s kept %v, want all four runs",
					cutoff.Format(time.RFC3339), time.UnixMilli(h).UTC().Format(time.RFC3339), got)
			}
		}
		first := from.In(loc)
		for day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
			start, end := day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli()
			var inDay, held []string
			for _, origin := range order {
				if origin >= start && origin < end {
					inDay = append(inDay, ids[origin])
					if holders[ids[origin]] {
						held = append(held, ids[origin])
					}
				}
			}
			if len(inDay) == 0 {
				continue
			}
			if end <= cut {
				if len(held) != 1 || held[0] != inDay[0] {
					t.Fatalf("pass %s: complete day %s has state on %v, want only %s",
						cutoff.Format(time.RFC3339), day.Format("2006-01-02"), held, inDay[0])
				}
			} else if len(held) != len(inDay) {
				t.Fatalf("pass %s: open day %s lost state references: %d of %d runs keep them",
					cutoff.Format(time.RFC3339), day.Format("2006-01-02"), len(held), len(inDay))
			}
		}
	}
	return arch
}

// Each daily pass looks for the next day left to thin. Through the origin
// index that walks every thinned run first; the partial index holds only
// the runs still to thin.
func TestForecastLongThinPlansUseIndexes(t *testing.T) {
	_, arch := openLongArchive(t)
	span := []any{int64(0), int64(1)}
	assertPlanSearches(t, arch, forecastLongNextUnthinned, span, "fa_runs", "fa_runs_unthinned")
	assertPlanSearches(t, arch, forecastLongHourLosers, span, "fa_runs", "fa_runs_origin")
	assertPlanSearches(t, arch, forecastLongDayRuns, span, "fa_runs", "fa_runs_origin")
}
