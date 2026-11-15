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
	if err := arch.pruneToCap(ctx, now); err != nil {
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
