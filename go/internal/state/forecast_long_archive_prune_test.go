package state

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastLongRetentionAndCap(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	arch.retention = 48 * time.Hour
	arch.fullDetail = 24 * time.Hour
	old := longIssue("old", now.Add(-72*time.Hour))
	recent := longIssue("recent", now.Add(-time.Hour))
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: old}, {Issue: recent}}); err != nil {
		t.Fatal(err)
	}
	obs := func(at time.Time) forecasting.Observation {
		return forecasting.Observation{
			StartMS: at.UnixMilli(), EndMS: at.Add(15 * time.Minute).UnixMilli(),
			AvailableAtMS: at.Add(15 * time.Minute).UnixMilli(), PVW: 1, LoadW: 2,
			PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: "cfg-v1",
		}
	}
	if err := arch.ArchiveObservations(ctx, []forecasting.Observation{obs(now.Add(-72 * time.Hour)), obs(now.Add(-time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := arch.pruneExpired(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"recent": true})
	var oldObs int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_observations_15m WHERE end_ms<?`, now.Add(-48*time.Hour).UnixMilli()).Scan(&oldObs); err != nil || oldObs != 0 {
		t.Fatalf("expired observations = %d err=%v", oldObs, err)
	}
	arch.maxBytes = 1
	bulk := longIssue("bulk", now.Add(-72*time.Hour))
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: bulk}}); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.pruneToCap(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"recent": true})
	if arch.status.State != "over_cap" {
		t.Fatalf("state = %s, want over_cap while the newest day still exceeds the cap", arch.status.State)
	}
}

func TestForecastLongIncrementalVacuumShrinksFile(t *testing.T) {
	_, arch := openLongArchive(t)
	ctx := context.Background()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if _, err := arch.db.ExecContext(ctx, `INSERT INTO fa_model_states(id, created_at_ms, expanded_bytes, payload) VALUES('pad', 1, ?, ?)`, len(payload), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	before, err := arch.liveByteCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := regularFileSize(arch.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `DELETE FROM fa_model_states WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := arch.incrementalVacuum(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	after, err := regularFileSize(arch.path)
	if err != nil {
		t.Fatal(err)
	}
	var free int64
	if err := arch.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if after >= infoBefore && free > 0 {
		t.Fatalf("vacuum left the file at %d (was %d) with %d free pages; live before delete %d", after, infoBefore, free, before)
	}
}

func assertRuns(t *testing.T, arch *ForecastLongArchive, want map[string]bool) {
	t.Helper()
	rows, err := arch.db.Query(`SELECT run_id FROM fa_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if len(got) != len(want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("missing %s in %v", id, got)
		}
	}
}

func TestForecastLongBelowFloorStillThinsAndGivesSpaceBack(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	hour := now.Add(-40 * 24 * time.Hour)
	later := longIssue("old-later", hour)
	later.IssuedAtMS += 1000
	// The pad day is the oldest outside the 30 days. Deleting it gives back
	// far more than the 64 KiB shortfall, so the thinned day stays.
	pad := ForecastArchiveIssue{
		Issue:  longIssue("pad", now.Add(-60*24*time.Hour)),
		States: map[string]ForecastModelBlob{"pad-state": {Expanded: 2 << 20, Gzip: make([]byte, 2<<20)}},
	}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{
		{Issue: longIssue("expired", now.Add(-800*24*time.Hour))}, pad,
		{Issue: longIssue("old-keep", hour)}, {Issue: later},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastIssue(ctx, longIssue("fresh", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := arch.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	before, err := regularFileSize(arch.path)
	if err != nil {
		t.Fatal(err)
	}
	restore := ForecastLongDiskAvailForTest(func(string) (int64, error) { return arch.minFree - 64<<10, nil })
	defer restore()
	if err := arch.Sync(ctx, store, now); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"old-keep": true})
	var thinned int
	if err := arch.db.QueryRow(`SELECT thinned FROM fa_runs WHERE run_id='old-keep'`).Scan(&thinned); err != nil || thinned != 1 {
		t.Fatalf("old-keep thinned = %d err=%v", thinned, err)
	}
	var pads int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_model_states WHERE id='pad-state'`).Scan(&pads); err != nil || pads != 0 {
		t.Fatalf("pad state rows = %d err=%v", pads, err)
	}
	status, _ := store.ForecastLongArchiveStatus()
	if status.State != "paused_disk_full" {
		t.Fatalf("state = %q, want paused_disk_full", status.State)
	}
	var free, page int64
	if err := arch.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := arch.db.QueryRow(`PRAGMA page_size`).Scan(&page); err != nil {
		t.Fatal(err)
	}
	after, err := regularFileSize(arch.path)
	if err != nil {
		t.Fatal(err)
	}
	if free*page >= forecastLongFreeTarget || after > before-(1<<20) {
		t.Fatalf("file %d bytes (was %d) with %d free pages; the pad pages were not given back", after, before, free)
	}
}

func TestForecastLongHourlySyncEnforcesCap(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	restore := ForecastLongDiskAvailForTest(func(string) (int64, error) { return 1 << 40, nil })
	defer restore()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	// The first copy runs the daily pass; the next is 24 hours away.
	if err := arch.Sync(ctx, store, now); err != nil {
		t.Fatal(err)
	}
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{
		{Issue: longIssue("old", now.Add(-40*24*time.Hour))},
		{Issue: longIssue("recent", now.Add(-time.Hour))},
	}); err != nil {
		t.Fatal(err)
	}
	arch.maxBytes = 1
	if err := arch.Sync(ctx, store, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertRuns(t, arch, map[string]bool{"recent": true})
	if status, _ := store.ForecastLongArchiveStatus(); status.State != "over_cap" {
		t.Fatalf("state = %q, want over_cap an hour after the daily pass", status.State)
	}
	arch.maxBytes = ForecastLongMaxBytes
	if err := arch.Sync(ctx, store, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if status, _ := store.ForecastLongArchiveStatus(); status.State != "ok" {
		t.Fatalf("state = %q, want ok once the archive is under the cap", status.State)
	}
}
