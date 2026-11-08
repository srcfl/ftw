package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/journal"
	"github.com/srcfl/ftw/go/internal/state"
)

func openJournalStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A stop ends the spans now and writes them; the next start finds none
// left open.
func TestStopControlJournalClosesAndWrites(t *testing.T) {
	st := openJournalStore(t)
	rec := newControlJournal(st)
	now := time.Now()
	rec.Observe(now, []journal.Verdict{{Subject: "site:dispatch", Kind: "dispatch", Status: "free", Severity: "info"}})
	stopControlJournal(rec)
	spans, _, err := st.ControlSpans(context.Background(), state.ControlSpanQuery{SinceMs: 0, UntilMs: now.Add(time.Minute).UnixMilli()})
	if err != nil || len(spans) != 1 || spans[0].EndMs == 0 {
		t.Fatalf("spans %+v err %v", spans, err)
	}
	if n, _ := st.CloseOpenControlSpans(context.Background()); n != 0 {
		t.Fatalf("%d spans left open after a clean stop", n)
	}
}

// events.get for one source reads that source's spans and notices only.
func TestStateJournalKeepsOneSource(t *testing.T) {
	st := openJournalStore(t)
	ctx := context.Background()
	spans := []*state.ControlSpan{
		{Subject: "battery:sungrow", Driver: "sungrow", Kind: "battery", Status: "limited", Severity: "warning", StartMs: 100, UpdatedMs: 200},
		{Subject: "ev:easee", Driver: "easee", Kind: "ev", Status: "following", Severity: "info", StartMs: 100, UpdatedMs: 200},
	}
	spans[0].Actual.Add(ptrW(1250))
	if err := st.SaveControlSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"sungrow", "easee"} {
		if err := st.RecordNotification(state.NotificationEntry{TsMs: 150, EventType: "control.not_following", Driver: d, Status: "sent"}); err != nil {
			t.Fatal(err)
		}
	}
	got, notices, more, err := stateJournal{st: st}.Journal(ctx, appproto.JournalQuery{SinceMs: 0, UntilMs: 1_000, Driver: "sungrow", Limit: 50})
	if err != nil || more || len(got) != 1 || got[0].Kind != "battery" || got[0].Actual == nil || got[0].Actual.Mean != 1250 || got[0].Requested != nil {
		t.Fatalf("spans %+v more %v err %v", got, more, err)
	}
	if len(notices) != 1 || notices[0].Driver != "sungrow" {
		t.Fatalf("notices %+v", notices)
	}
}

// A locked database delays a notification row; it does not lose it.
func TestRecordWithRetryWritesAfterAFailure(t *testing.T) {
	saved := notificationLogRetries
	notificationLogRetries = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { notificationLogRetries = saved })
	var tries atomic.Int32
	done := make(chan struct{})
	recordWithRetry("notification_log", func() error {
		if tries.Add(1) < 3 {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		close(done)
		return nil
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("not written after %d tries", tries.Load())
	}
}

func ptrW(v float64) *float64 { return &v }
