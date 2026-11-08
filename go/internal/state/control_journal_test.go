package state

import (
	"context"
	"encoding/json"
	"testing"
)

func jw(v float64) *float64 { return &v }

func TestControlSpansRoundTripAndWindow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	limited := &ControlSpan{Subject: "battery:sungrow", Driver: "sungrow", Kind: "battery",
		Status: "limited", Reason: "power_below_target", Severity: "warning", Mode: "planner_arbitrage",
		StartMs: 1_000, UpdatedMs: 1_000, Samples: 1, First: json.RawMessage(`{"requested_w":4200}`)}
	limited.Requested.Add(jw(4200))
	limited.Actual.Add(jw(1200))
	if err := st.SaveControlSpans(ctx, []*ControlSpan{limited}); err != nil {
		t.Fatal(err)
	}
	if limited.ID == 0 {
		t.Fatal("insert did not set the id")
	}
	limited.Samples, limited.UpdatedMs, limited.EndMs = 3, 9_000, 10_000
	limited.Requested.Add(jw(4300))
	limited.Actual.Add(nil)
	limited.Last = json.RawMessage(`{"requested_w":4300}`)
	quiet := &ControlSpan{Subject: "source:myuplink", Driver: "myuplink", Kind: "source",
		Status: "stale", Severity: "warning", StartMs: 20_000, UpdatedMs: 20_000, Samples: 1}
	if err := st.SaveControlSpans(ctx, []*ControlSpan{limited, quiet}); err != nil {
		t.Fatal(err)
	}

	got, more, err := st.ControlSpans(ctx, ControlSpanQuery{SinceMs: 5_000, UntilMs: 6_000})
	if err != nil || more || len(got) != 1 {
		t.Fatalf("window inside the span: got %+v more=%v err=%v", got, more, err)
	}
	sp := got[0]
	if sp.EndMs != 10_000 || sp.Samples != 3 || sp.Requested.N != 2 || sp.Requested.Max != 4300 || sp.Actual.N != 1 || sp.Actual.Min != 1200 {
		t.Fatalf("stats did not round-trip: %+v", sp)
	}
	if mean, ok := sp.Requested.Mean(); !ok || mean != 4250 {
		t.Fatalf("requested mean = %v, %v", mean, ok)
	}
	if string(sp.First) != `{"requested_w":4200}` || string(sp.Last) != `{"requested_w":4300}` {
		t.Fatalf("verdict text lost: first %s last %s", sp.First, sp.Last)
	}

	// An open span overlaps every window after its start.
	got, _, _ = st.ControlSpans(ctx, ControlSpanQuery{SinceMs: 50_000, UntilMs: 60_000})
	if len(got) != 1 || got[0].Subject != "source:myuplink" || got[0].EndMs != 0 {
		t.Fatalf("open span: %+v", got)
	}
	got, _, _ = st.ControlSpans(ctx, ControlSpanQuery{SinceMs: 0, UntilMs: 60_000, SubjectPrefix: "battery:", Severities: []string{"warning"}})
	if len(got) != 1 || got[0].Subject != "battery:sungrow" {
		t.Fatalf("prefix and severity filter: %+v", got)
	}
	got, more, _ = st.ControlSpans(ctx, ControlSpanQuery{SinceMs: 0, UntilMs: 60_000, Limit: 1})
	if len(got) != 1 || !more || got[0].Subject != "source:myuplink" {
		t.Fatalf("limit keeps the newest and says more: %+v more=%v", got, more)
	}
}

// A restart leaves spans open. Nothing is known after the last update, so
// they end there rather than run on through the outage.
func TestCloseOpenControlSpansEndsAtLastUpdate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	open := &ControlSpan{Subject: "site:dispatch", Kind: "dispatch", Status: "free", StartMs: 1_000, UpdatedMs: 7_000}
	if err := st.SaveControlSpans(ctx, []*ControlSpan{open}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CloseOpenControlSpans(ctx); err != nil || n != 1 {
		t.Fatalf("closed %d, err %v", n, err)
	}
	got, _, _ := st.ControlSpans(ctx, ControlSpanQuery{SinceMs: 0, UntilMs: 10_000})
	if len(got) != 1 || got[0].EndMs != 7_000 {
		t.Fatalf("span not closed at its last update: %+v", got)
	}
	if n, err := st.PruneControlSpans(ctx, 8_000); err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
}

// A failed write leaves new spans without an id, so the next try inserts
// them again instead of updating rows that were never written.
func TestSaveControlSpansKeepsIDsOnFailure(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sp := &ControlSpan{Subject: "battery:sungrow", Status: "following", StartMs: 1, UpdatedMs: 1}
	if err := st.SaveControlSpans(ctx, []*ControlSpan{sp}); err == nil {
		t.Fatal("a cancelled write must fail")
	}
	if sp.ID != 0 {
		t.Fatalf("id %d set by a failed write", sp.ID)
	}
	if err := st.SaveControlSpans(context.Background(), []*ControlSpan{sp}); err != nil || sp.ID == 0 {
		t.Fatalf("retry: id %d err %v", sp.ID, err)
	}
}

func TestNotificationsBetween(t *testing.T) {
	st := openTestStore(t)
	for i, ts := range []int64{1_000, 5_000, 9_000} {
		if err := st.RecordNotification(NotificationEntry{TsMs: ts, EventType: "control.not_following", Status: "sent", Priority: i}); err != nil {
			t.Fatal(err)
		}
	}
	got, more, err := st.NotificationsBetween(context.Background(), 2_000, 9_000, 1)
	if err != nil || !more || len(got) != 1 || got[0].TsMs != 9_000 {
		t.Fatalf("got %+v more=%v err=%v", got, more, err)
	}
}
