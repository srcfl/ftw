package state

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastLongCopyPagesAndResumes(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	origin := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Hour)
	const n = 20
	for i := 0; i < n; i++ {
		issue := longIssue("run-"+forecastLongItoa(i), origin.Add(time.Duration(i)*15*time.Minute))
		if err := store.SaveForecastIssue(ctx, issue); err != nil {
			t.Fatal(err)
		}
	}
	obs := forecasting.Observation{
		StartMS: origin.UnixMilli(), EndMS: origin.Add(15 * time.Minute).UnixMilli(),
		AvailableAtMS: origin.Add(15 * time.Minute).UnixMilli(), PVW: 10, LoadW: 20,
		PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: "cfg-v1",
	}
	if err := store.SaveForecastObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	pages := 0
	forecastLongAfterIssuePage = func(rows int) error {
		pages++
		if pages == 1 {
			return errors.New("stop after the first page")
		}
		return nil
	}
	t.Cleanup(func() { forecastLongAfterIssuePage = nil })
	if err := arch.Sync(ctx, store, time.Now().UTC()); err == nil {
		t.Fatal("interrupted copy reported success")
	}
	var runs int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != forecastLongIssuePage {
		t.Fatalf("first page stored %d runs, want %d", runs, forecastLongIssuePage)
	}
	forecastLongAfterIssuePage = nil
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != n {
		t.Fatalf("resumed runs = %d err=%v, want %d", runs, err, n)
	}
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != n {
		t.Fatalf("second copy duplicated runs: %d err=%v", runs, err)
	}
	var observations int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_observations_15m`).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("observations = %d err=%v", observations, err)
	}
	var srcGzip, dstGzip []byte
	if err := store.db.QueryRow(`SELECT payload FROM forecast_model_states LIMIT 1`).Scan(&srcGzip); err != nil {
		t.Fatal(err)
	}
	if err := arch.db.QueryRow(`SELECT payload FROM fa_model_states LIMIT 1`).Scan(&dstGzip); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(srcGzip, dstGzip) {
		t.Fatal("model state was recompressed")
	}
	var states int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_model_states`).Scan(&states); err != nil || states != 1 {
		t.Fatalf("model states = %d err=%v, want the one shared snapshot", states, err)
	}
}

func TestForecastLongCopySkipsPrunedSource(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	issue := longIssue("kept", time.Now().UTC().Add(-time.Hour).Truncate(time.Hour))
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM forecast_model_states`); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var runs, states int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs = %d err=%v", runs, err)
	}
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_model_states`).Scan(&states); err != nil || states != 0 {
		t.Fatalf("pruned model state was invented: %d err=%v", states, err)
	}
	if _, err := store.db.Exec(`DELETE FROM forecast_issues`); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("archive dropped a copied run after the source was pruned: %d err=%v", runs, err)
	}
}

func forecastLongItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func copiedRunIDs(t *testing.T, arch *ForecastLongArchive) map[string]bool {
	t.Helper()
	rows, err := arch.db.Query(`SELECT run_id FROM fa_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

func longObservation(start time.Time, availableAfterEnd time.Duration) forecasting.Observation {
	end := start.Add(15 * time.Minute)
	return forecasting.Observation{
		StartMS: start.UnixMilli(), EndMS: end.UnixMilli(), AvailableAtMS: end.Add(availableAfterEnd).UnixMilli(),
		PVW: 10, LoadW: 20, PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: "cfg-v1",
	}
}

func TestForecastLongCopyKeepsLateRows(t *testing.T) {
	if forecastLongCopyOverlap < maxForecastFutureSkew {
		t.Fatal("the copy overlap must cover the future skew state.db accepts")
	}
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	origin := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	if err := store.SaveForecastIssue(ctx, longIssue("b-primary", origin)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastObservation(ctx, longObservation(origin, time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The shadow has the same issued time and an id that sorts first. The
	// earlier quarter was released after the later one.
	if err := store.SaveForecastIssue(ctx, longIssue("a-shadow", origin)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastObservation(ctx, longObservation(origin.Add(-15*time.Minute), 20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if runs := copiedRunIDs(t, arch); !runs["a-shadow"] || !runs["b-primary"] {
		t.Fatalf("runs = %v, the late shadow was skipped", runs)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_observations_15m`); n != 2 {
		t.Fatalf("observations = %d, the late quarter was skipped", n)
	}
}

func TestForecastLongCopyRecoversFromFutureCursor(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	origin := now.Truncate(time.Hour).Add(-2 * time.Hour)
	if err := store.SaveForecastIssue(ctx, longIssue("real", origin)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastObservation(ctx, longObservation(origin, time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A row stamped by a clock a day ahead. state.db keeps it, since its
	// retention counts back from now.
	ahead, err := prepareForecastIssue(longIssue("ahead", now.Truncate(time.Hour).Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO forecast_issues(id,origin_ms,issued_at_ms,config_version,payload) VALUES(?,?,?,?,?)`,
		ahead.issue.ID, ahead.issue.OriginMS, ahead.issue.IssuedAtMS, ahead.issue.ConfigVersion, ahead.payload); err != nil {
		t.Fatal(err)
	}
	// Cursors written while the clock was a year ahead.
	yearAhead := now.Add(365 * 24 * time.Hour).UnixMilli()
	if err := arch.writeIssueCursor(ctx, forecastLongIssueCursor{IssuedAtMS: yearAhead, ID: "z"}); err != nil {
		t.Fatal(err)
	}
	if err := arch.writeObsCursor(ctx, forecastLongObsCursor{AvailableMS: yearAhead}); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, now); err != nil {
		t.Fatal(err)
	}
	if runs := copiedRunIDs(t, arch); len(runs) != 1 || !runs["real"] {
		t.Fatalf("runs = %v, want only the run issued before now", runs)
	}
	if n := countRows(t, arch, `SELECT COUNT(*) FROM fa_observations_15m`); n != 1 {
		t.Fatalf("observations = %d after the cursor reset", n)
	}
	bound := now.Add(maxForecastFutureSkew).UnixMilli()
	issueCursor, err := arch.issueCursor(ctx)
	if err != nil || issueCursor.IssuedAtMS > bound {
		t.Fatalf("issue cursor = %+v err=%v, ahead of the clock again", issueCursor, err)
	}
	obsCursor, err := arch.obsCursor(ctx)
	if err != nil || obsCursor.AvailableMS > bound {
		t.Fatalf("observation cursor = %+v err=%v, ahead of the clock again", obsCursor, err)
	}

	// A rescan skips stored runs by id and never reads their payloads.
	if _, err := store.db.Exec(`UPDATE forecast_issues SET payload=x'00' WHERE id='real'`); err != nil {
		t.Fatal(err)
	}
	if err := arch.writeIssueCursor(ctx, forecastLongIssueCursor{IssuedAtMS: yearAhead}); err != nil {
		t.Fatal(err)
	}
	if err := arch.Sync(ctx, store, now); err != nil {
		t.Fatal(err)
	}
	if status, _ := store.ForecastLongArchiveStatus(); status.LastError != "" {
		t.Fatalf("rescan decoded a stored run: %q", status.LastError)
	}

	// The row stamped ahead is copied once the clock reaches it.
	if err := arch.Sync(ctx, store, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if runs := copiedRunIDs(t, arch); !runs["ahead"] {
		t.Fatalf("runs = %v, the row stamped ahead was never copied", runs)
	}
}

func TestForecastLongCopyQueriesUseIndexes(t *testing.T) {
	store, _ := openLongArchive(t)
	if err := store.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		query, index string
		args         []any
	}{
		{forecastIssueKeysQuery, "forecast_issues_time", []any{1, "a", 2, 16}},
		{forecastObservationsAfterQuery, "forecast_observations_time", []any{1, 2, 3, "c", 5, 128}},
	} {
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+q.query, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if strings.Contains(joined, "SCAN ") || !strings.Contains(joined, "USING INDEX "+q.index) {
			t.Fatalf("plan for the copy query = %s", joined)
		}
	}
}
