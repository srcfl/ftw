package journal

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// fakeStore keeps the latest copy of each span by id, as the table would.
type fakeStore struct {
	rows   map[int64]state.ControlSpan
	nextID int64
	fail   bool
	writes int
}

func (f *fakeStore) SaveControlSpans(_ context.Context, spans []*state.ControlSpan) error {
	f.writes++
	if f.fail {
		return errors.New("database is locked")
	}
	if f.rows == nil {
		f.rows = map[int64]state.ControlSpan{}
	}
	for _, sp := range spans {
		if sp.ID == 0 {
			f.nextID++
			sp.ID = f.nextID
		}
		f.rows[sp.ID] = *sp
	}
	return nil
}

func (f *fakeStore) bySubject(subject string) []state.ControlSpan {
	var out []state.ControlSpan
	for id := int64(1); id <= f.nextID; id++ {
		if sp, ok := f.rows[id]; ok && sp.Subject == subject {
			out = append(out, sp)
		}
	}
	return out
}

func p(v float64) *float64 { return &v }

func battery(status, reason, severity string, requested, actual float64) Verdict {
	detail, _ := json.Marshal(map[string]any{"status": status, "requested_w": requested, "actual_w": actual})
	return Verdict{Subject: "battery:sungrow", Driver: "sungrow", Kind: "battery", Status: status, Reason: reason,
		Severity: severity, Mode: "planner_arbitrage", RequestedW: p(requested), ActualW: p(actual), Detail: detail}
}

// The morning of 8 November 2026: the battery was ordered 4.2 kW and took
// 1.2 kW for a quarter of an hour. The journal keeps the stretch, its
// numbers and the verdict before and after it.
func TestRecorderTilesAVerdictsHistory(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	t0 := time.Date(2026, 11, 8, 6, 0, 0, 0, time.Local)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	r.Observe(at(0), []Verdict{battery("following", "plan", "info", 2440, 2440)})
	r.Flush(context.Background(), at(0))
	for m := 14; m <= 27; m++ {
		r.Observe(at(m), []Verdict{battery("limited", "power_below_target", "warning", 4250, 1250)})
		r.Flush(context.Background(), at(m))
	}
	// The return to an info verdict counts once it has held for Settle,
	// and from when it was first seen.
	r.Observe(at(30), []Verdict{battery("following", "idle", "info", 0, 0)})
	r.Flush(context.Background(), at(30))
	r.Observe(at(30).Add(Settle), []Verdict{battery("following", "idle", "info", 0, 0)})
	r.Flush(context.Background(), at(30).Add(Settle))

	spans := st.bySubject("battery:sungrow")
	if len(spans) != 3 {
		t.Fatalf("want 3 spans, got %+v", spans)
	}
	first, warn, last := spans[0], spans[1], spans[2]
	if first.EndMs != warn.StartMs || warn.EndMs != last.StartMs || last.EndMs != 0 {
		t.Fatalf("spans do not tile: %d-%d, %d-%d, %d-", first.StartMs, first.EndMs, warn.StartMs, warn.EndMs, last.StartMs)
	}
	if warn.StartMs != at(14).UnixMilli() || warn.EndMs != at(30).UnixMilli() || warn.Severity != "warning" || warn.Samples != 14 {
		t.Fatalf("warning span: %+v", warn)
	}
	if mean, _ := warn.Requested.Mean(); mean != 4250 {
		t.Fatalf("requested mean %v", mean)
	}
	if warn.Actual.Max != 1250 || len(warn.First) == 0 || len(warn.Last) == 0 {
		t.Fatalf("actual or verdict text lost: %+v", warn)
	}
}

// A subject that drops out of the sample, such as a driver removed from
// the config, has its span closed rather than left running.
func TestRecorderClosesASubjectThatWent(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	t0 := time.Unix(1_000, 0)
	r.Observe(t0, []Verdict{{Subject: "source:easee", Kind: "source", Status: "live", Severity: "info"}})
	r.Observe(t0.Add(5*time.Second), nil)
	r.Flush(context.Background(), t0.Add(5*time.Second))
	got := st.bySubject("source:easee")
	if len(got) != 1 || got[0].EndMs != t0.Add(5*time.Second).UnixMilli() {
		t.Fatalf("span not closed: %+v", got)
	}
	if len(r.Open()) != 0 {
		t.Fatalf("still open: %+v", r.Open())
	}
}

// A locked database must not lose a verdict: the spans wait in memory and
// the next flush writes them, with the change that came in between.
func TestRecorderKeepsSpansThroughFailedWrites(t *testing.T) {
	st := &fakeStore{fail: true}
	r := New(st)
	t0 := time.Unix(10_000, 0)
	r.Observe(t0, []Verdict{battery("following", "plan", "info", 1000, 1000)})
	r.Flush(context.Background(), t0)
	r.Observe(t0.Add(5*time.Second), []Verdict{battery("not_following", "no_power_response", "warning", 1000, 0)})
	r.Flush(context.Background(), t0.Add(5*time.Second))
	if len(st.rows) != 0 {
		t.Fatalf("a failing store took rows: %+v", st.rows)
	}
	st.fail = false
	r.Flush(context.Background(), t0.Add(10*time.Second))
	spans := st.bySubject("battery:sungrow")
	if len(spans) != 2 || spans[0].EndMs != spans[1].StartMs || spans[1].Status != "not_following" {
		t.Fatalf("spans after recovery: %+v", spans)
	}
	writes := st.writes
	r.Flush(context.Background(), t0.Add(15*time.Second))
	if st.writes != writes {
		t.Fatal("nothing changed, yet the recorder wrote again")
	}
}

// A span that holds writes its running numbers once a StatsEvery, so a
// crash loses at most that much of them.
func TestRecorderWritesHoldingSpansOnceAStatsEvery(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	t0 := time.Unix(20_000, 0)
	r.Observe(t0, []Verdict{battery("following", "plan", "info", 500, 500)})
	r.Flush(context.Background(), t0)
	step := 5 * time.Second
	n := int(StatsEvery / step)
	for i := 1; i < n; i++ {
		at := t0.Add(time.Duration(i) * step)
		r.Observe(at, []Verdict{battery("following", "plan", "info", 500, 500)})
		r.Flush(context.Background(), at)
	}
	if got := st.bySubject("battery:sungrow")[0].Samples; got != 1 {
		t.Fatalf("samples written before StatsEvery: %d", got)
	}
	at := t0.Add(StatsEvery)
	r.Observe(at, []Verdict{battery("following", "plan", "info", 500, 500)})
	r.Flush(context.Background(), at)
	if got := st.bySubject("battery:sungrow")[0]; got.Samples != int64(n+1) || got.UpdatedMs != at.UnixMilli() {
		t.Fatalf("after StatsEvery: %+v", got)
	}
}

func TestRecorderRecordsAFinishedSpan(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	r.Record(state.ControlSpan{Subject: "slot:batteries", Kind: "slot", Status: "under_delivered", Severity: "warning", StartMs: 1, EndMs: 900_001, UpdatedMs: 900_001})
	r.Flush(context.Background(), time.Unix(1_000, 0))
	if got := st.bySubject("slot:batteries"); len(got) != 1 || got[0].Status != "under_delivered" {
		t.Fatalf("slot span: %+v", got)
	}
}

// Each new order leaves a device "waiting" for a moment. A blip to an info
// verdict shorter than Settle starts no span; a warning does, however short.
func TestRecorderSettlesInfoChangesButNotWarnings(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	t0 := time.Unix(30_000, 0)
	tick := func(s int, v Verdict) {
		at := t0.Add(time.Duration(s) * time.Second)
		r.Observe(at, []Verdict{v})
		r.Flush(context.Background(), at)
	}
	tick(0, battery("following", "plan", "info", 1000, 1000))
	tick(5, battery("waiting", "waiting_response", "info", 1200, 1000))
	tick(10, battery("following", "plan", "info", 1200, 1200))
	if got := st.bySubject("battery:sungrow"); len(got) != 1 {
		t.Fatalf("a 5 s info blip started a span: %+v", got)
	}
	tick(15, battery("not_following", "no_power_response", "warning", 1200, 0))
	tick(18, battery("following", "plan", "info", 1200, 1200))
	tick(33, battery("following", "plan", "info", 1200, 1200))
	got := st.bySubject("battery:sungrow")
	if len(got) != 3 || got[1].Status != "not_following" ||
		got[1].StartMs != t0.Add(15*time.Second).UnixMilli() || got[1].EndMs != t0.Add(18*time.Second).UnixMilli() {
		t.Fatalf("a 3 s warning must be its own span, ended when the recovery was first seen: %+v", got)
	}
}

// blockingStore holds each write until released, and fails spans whose
// subject it refuses.
type blockingStore struct {
	mu      sync.Mutex
	inserts map[string]int
	nextID  int64
	gate    chan struct{}
	refuse  string
}

func (b *blockingStore) SaveControlSpans(_ context.Context, spans []*state.ControlSpan) error {
	if b.gate != nil {
		<-b.gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sp := range spans {
		if sp.Subject == b.refuse {
			return errors.New("NOT NULL constraint failed")
		}
	}
	for _, sp := range spans {
		if sp.ID == 0 {
			b.nextID++
			sp.ID = b.nextID
			b.inserts[sp.Subject]++
		}
	}
	return nil
}

// A stop flushes from another goroutine while the journal's own flush may
// be writing. A new span is inserted once, and samples after the stop
// start nothing.
func TestRecorderFlushesTakeTurns(t *testing.T) {
	st := &blockingStore{inserts: map[string]int{}, gate: make(chan struct{})}
	r := New(st)
	t0 := time.Unix(40_000, 0)
	r.Observe(t0, []Verdict{battery("following", "plan", "info", 500, 500)})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r.Flush(context.Background(), t0) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(st.gate)
	wg.Wait()
	if n := st.inserts["battery:sungrow"]; n != 1 {
		t.Fatalf("span inserted %d times", n)
	}
	r.CloseAll(t0.Add(time.Second))
	r.Observe(t0.Add(2*time.Second), []Verdict{battery("following", "plan", "info", 500, 500)})
	if open := r.Open(); len(open) != 0 {
		t.Fatalf("a sample after the stop opened %+v", open)
	}
}

// One span no write will take does not hold back the others, and is
// dropped once it has failed long enough.
func TestRecorderIsolatesASpanNoWriteTakes(t *testing.T) {
	st := &blockingStore{inserts: map[string]int{}, refuse: "source:bad"}
	r := New(st)
	t0 := time.Unix(50_000, 0)
	r.Observe(t0, []Verdict{
		{Subject: "source:bad", Kind: "source", Status: "live", Severity: "info"},
		battery("following", "plan", "info", 500, 500),
	})
	r.Observe(t0.Add(5*time.Second), []Verdict{battery("following", "plan", "info", 500, 500)})
	for i := range giveUpAfter + 1 {
		r.Flush(context.Background(), t0.Add(time.Duration(10+i)*time.Second))
	}
	if st.inserts["battery:sungrow"] != 1 {
		t.Fatalf("the good span was held back: %+v", st.inserts)
	}
	r.mu.Lock()
	left := len(r.pending)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d spans still pending after giving up", left)
	}
}

// A warning that arrives while an info change waits starts its span at
// once, and the change that never settled leaves no trace.
func TestRecorderWarningDuringACandidate(t *testing.T) {
	st := &fakeStore{}
	r := New(st)
	t0 := time.Unix(60_000, 0)
	tick := func(s int, v Verdict) {
		at := t0.Add(time.Duration(s) * time.Second)
		r.Observe(at, []Verdict{v})
		r.Flush(context.Background(), at)
	}
	tick(0, battery("following", "plan", "info", 1000, 1000))
	tick(5, battery("waiting", "waiting_response", "info", 1000, 0))
	tick(10, battery("not_following", "no_power_response", "warning", 1000, 0))
	got := st.bySubject("battery:sungrow")
	if len(got) != 2 || got[0].Status != "following" || got[0].EndMs != t0.Add(10*time.Second).UnixMilli() || got[1].Status != "not_following" {
		t.Fatalf("spans %+v", got)
	}
}
