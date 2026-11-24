package main

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func forecastingIssueForArchive(origin time.Time) forecasting.Issue {
	start := origin.UnixMilli()
	band := forecasting.Band{Method: forecasting.BandMethodColdStart}
	point := forecasting.Point{
		StartMS: start, EndMS: start + int64(time.Hour/time.Millisecond),
		PVW: 1, LoadW: 2, PVKnown: true, LoadKnown: true,
		PVQuality: "ready", LoadQuality: "ready",
		PVBand: band, LoadBand: band, NetBand: band,
	}
	return forecasting.Issue{
		Schema: forecasting.Schema, ID: "issued", DecisionID: "decision",
		OriginMS: start, IssuedAtMS: start, LatestInputMS: start, ConfigVersion: "cfg",
		Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: []forecasting.Point{point}}},
	}
}

func TestForecastLongArchiverStops(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	archiver := &forecastLongArchiver{store: st, startDelay: 0, interval: time.Hour}
	archiver.Start(ctx)
	archiver.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines leaked: before %d after %d", before, got)
	}
}

func TestForecastLongArchiverPausesWhenDiskIsFull(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	issue := forecastingIssueForArchive(origin)
	if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	restore := state.ForecastLongDiskAvailForTest(func(string) (int64, error) { return 0, nil })
	defer restore()
	archiver := &forecastLongArchiver{store: st, startDelay: time.Hour, interval: time.Hour}
	archiver.runOnce(context.Background())
	status, ok := st.ForecastLongArchiveStatus()
	if !ok || status.State != "paused_disk_full" || status.Runs != 0 {
		t.Fatalf("status = %+v ok=%v", status, ok)
	}
}

func TestForecastRecordDoesNotWaitOnLongArchive(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	if err := st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveForecastIssue(context.Background(), forecastingIssueForArchive(origin)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	restore := state.ForecastLongDiskAvailForTest(func(string) (int64, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return 1 << 40, nil
	})
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	archiver := &forecastLongArchiver{store: st, startDelay: 0, interval: time.Hour}
	archiver.Start(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("copier did not reach a write")
	}
	f := trackerFixture(origin)
	f.store = st
	in := f.Snapshot(origin, trackerWeather(origin.Add(time.Minute), origin))
	slots := trackerSlots(origin.Add(time.Minute), 1)
	started := time.Now()
	for i := 0; i < 30; i++ {
		in.Record(slots, slots, nil, "decision", origin.UnixMilli())
	}
	if time.Since(started) > time.Second {
		t.Fatal("planner record waited on the long archive")
	}
	close(release)
	archiver.Stop()
}
