package state

import (
	"bytes"
	"context"
	"errors"
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
