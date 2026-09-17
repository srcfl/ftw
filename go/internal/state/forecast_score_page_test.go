package state

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastScorePagesPreserveEqualTimestampsAndNewestIssue(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	origin := start - int64(time.Hour/time.Millisecond)
	for i := 0; i < 11; i++ {
		issue := archiveIssue(fmt.Sprintf("issue-%02d", i), origin, start)
		issue.Series[0].Points[0].PVW = float64(i)
		if err := store.SaveForecastIssue(ctx, issue); err != nil {
			t.Fatal(err)
		}
	}
	observation := forecasting.Observation{StartMS: start, EndMS: start + int64(time.Hour/time.Millisecond), AvailableAtMS: start + int64(time.Hour/time.Millisecond), ConfigVersion: "cfg", Quality: "complete", PVW: 200, LoadW: 1200, PVKnown: true, LoadKnown: true}
	now := observation.AvailableAtMS
	var all []forecasting.Issue
	cursor := ForecastIssueCursor{}
	for {
		page, next, err := store.LoadForecastScorePage(ctx, origin, now, cursor, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		// Writing while paging must work even with one DB connection.
		scores := forecasting.Errors(page, []forecasting.Observation{observation}, now)
		if err := store.SaveForecastErrors(ctx, scores, now); err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		cursor = next
	}
	if len(all) != 11 {
		t.Fatalf("lost or repeated equal-timestamp rows: %d", len(all))
	}
	want := forecasting.Errors(all, []forecasting.Observation{observation}, now)
	got, err := store.LoadForecastErrors(ctx, start, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged scores differ: got %+v want %+v", got, want)
	}
	if len(got) != 1 || got[0].IssueID != "issue-10" {
		t.Fatalf("latest eligible issue did not win: %+v", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, next, err := store.LoadForecastScorePage(canceled, origin, now, cursor, 4); err == nil || next != cursor {
		t.Fatalf("failed read advanced cursor: %+v %v", next, err)
	}
}
