// Package journal records how the box's answer to "Are we in control?"
// changes, so a warning can be explained after it has passed. Every few
// seconds it takes each subject's verdict: a device function's control
// feedback, a source's freshness, whether the site may dispatch, the mode.
// A changed verdict closes the subject's span and opens the next, so the
// spans tile its history. A span carries the power Core asked for and the
// power measured while it held, and the verdict as Core served it.
package journal

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Verdict is one subject's state at one sample.
type Verdict struct {
	// Subject names what holds the verdict, e.g. "battery:sungrow".
	Subject  string
	Driver   string
	Kind     string
	Status   string
	Reason   string
	Severity string
	Mode     string
	// RequestedW and ActualW are nil when unknown, never an invented 0.
	RequestedW *float64
	ActualW    *float64
	// Detail is the verdict as Core served it, kept as the span's first
	// and latest record.
	Detail json.RawMessage
}

// Store saves spans. state.Store implements it.
type Store interface {
	SaveControlSpans(ctx context.Context, spans []*state.ControlSpan) error
}

const (
	// StatsEvery is how often a span that still holds writes its running
	// numbers. A changed verdict is written at the next flush.
	StatsEvery = time.Minute
	// maxPending bounds what a failing database leaves in memory.
	maxPending = 5000
	// failLogEvery spaces the warnings while writes keep failing.
	failLogEvery = 10 * time.Minute
	// Settle is how long a change to an info verdict must hold before it
	// starts a span. Each new order makes a device "waiting" for a second
	// or two; that is not a change worth a span. A warning or an alarm
	// starts one at once, however short, and a settled change is dated
	// from when it was first seen.
	Settle = 15 * time.Second
)

// Recorder turns samples into spans and writes them. Observe and Flush run
// on one goroutine; Record may be called from any.
type Recorder struct {
	mu    sync.Mutex
	store Store
	open  map[string]*state.ControlSpan
	// candidate holds a change to an info verdict until it settles.
	candidate map[string]*state.ControlSpan
	// pending holds spans with changes not yet written, each with the
	// version it was marked at, so a write of an older copy does not clear
	// a newer change.
	pending   map[*state.ControlSpan]uint64
	order     []*state.ControlSpan
	version   uint64
	statsAt   time.Time
	failingAt time.Time
	loggedAt  time.Time
	dropped   int
}

// New returns a recorder that writes to store.
func New(store Store) *Recorder {
	return &Recorder{store: store, open: map[string]*state.ControlSpan{}, candidate: map[string]*state.ControlSpan{},
		pending: map[*state.ControlSpan]uint64{}}
}

// Observe takes one sample of every subject. A subject missing from the
// sample has gone, and its span ends now.
func (r *Recorder) Observe(now time.Time, verdicts []Verdict) {
	if r == nil {
		return
	}
	nowMs := now.UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, v := range verdicts {
		if v.Subject == "" || seen[v.Subject] {
			continue
		}
		seen[v.Subject] = true
		sp := r.open[v.Subject]
		if sp != nil && sameVerdict(sp, v) {
			addSample(sp, v, nowMs)
			delete(r.candidate, v.Subject)
			continue
		}
		if sp == nil || v.Severity == "warning" || v.Severity == "alarm" {
			delete(r.candidate, v.Subject)
			r.startLocked(newSpan(v, nowMs), nowMs)
			continue
		}
		c := r.candidate[v.Subject]
		if c == nil || !sameVerdict(c, v) {
			r.candidate[v.Subject] = newSpan(v, nowMs)
			continue
		}
		addSample(c, v, nowMs)
		if nowMs-c.StartMs >= Settle.Milliseconds() {
			delete(r.candidate, v.Subject)
			r.startLocked(c, c.StartMs)
		}
	}
	for subject, sp := range r.open {
		if !seen[subject] {
			r.closeLocked(sp, nowMs)
			delete(r.open, subject)
			delete(r.candidate, subject)
		}
	}
}

// startLocked ends the subject's span at atMs and opens next.
func (r *Recorder) startLocked(next *state.ControlSpan, atMs int64) {
	if sp := r.open[next.Subject]; sp != nil {
		r.closeLocked(sp, atMs)
	}
	r.open[next.Subject] = next
	r.markLocked(next)
}

func sameVerdict(sp *state.ControlSpan, v Verdict) bool {
	return sp.Status == v.Status && sp.Reason == v.Reason && sp.Severity == v.Severity && sp.Mode == v.Mode
}

func newSpan(v Verdict, nowMs int64) *state.ControlSpan {
	sp := &state.ControlSpan{
		Subject: v.Subject, Driver: v.Driver, Kind: v.Kind,
		Status: v.Status, Reason: v.Reason, Severity: v.Severity, Mode: v.Mode,
		StartMs: nowMs, UpdatedMs: nowMs, Samples: 1, First: v.Detail, Last: v.Detail,
	}
	sp.Requested.Add(v.RequestedW)
	sp.Actual.Add(v.ActualW)
	return sp
}

func addSample(sp *state.ControlSpan, v Verdict, nowMs int64) {
	sp.Samples++
	sp.UpdatedMs = nowMs
	sp.Requested.Add(v.RequestedW)
	sp.Actual.Add(v.ActualW)
	if len(v.Detail) > 0 {
		sp.Last = v.Detail
	}
}

// Record adds a span that is already complete, such as a scored plan slot.
func (r *Recorder) Record(sp state.ControlSpan) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markLocked(&sp)
}

// CloseAll ends every open span now, for a clean shutdown.
func (r *Recorder) CloseAll(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for subject, sp := range r.open {
		r.closeLocked(sp, now.UnixMilli())
		delete(r.open, subject)
	}
	clear(r.candidate)
}

// Flush writes changed spans, and once a StatsEvery the running numbers of
// spans that still hold. A failed write keeps everything for the next one.
func (r *Recorder) Flush(ctx context.Context, now time.Time) {
	if r == nil || r.store == nil {
		return
	}
	type entry struct {
		orig    *state.ControlSpan
		version uint64
	}
	r.mu.Lock()
	var batch []entry
	inBatch := map[*state.ControlSpan]bool{}
	for _, sp := range r.order {
		if v, ok := r.pending[sp]; ok && !inBatch[sp] {
			batch = append(batch, entry{sp, v})
			inBatch[sp] = true
		}
	}
	if now.Sub(r.statsAt) >= StatsEvery {
		r.statsAt = now
		for _, sp := range r.open {
			if !inBatch[sp] {
				batch = append(batch, entry{sp, 0})
				inBatch[sp] = true
			}
		}
	}
	copies := make([]*state.ControlSpan, len(batch))
	for i, e := range batch {
		c := *e.orig
		copies[i] = &c
	}
	r.mu.Unlock()
	if len(copies) == 0 {
		return
	}

	err := r.store.SaveControlSpans(ctx, copies)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.failingAt.IsZero() {
			r.failingAt = now
		}
		if now.Sub(r.loggedAt) >= failLogEvery {
			r.loggedAt = now
			slog.Warn("control journal: write failed; keeping spans for the next try",
				"pending", len(r.pending), "since", r.failingAt, "err", err)
		}
		return
	}
	if !r.failingAt.IsZero() {
		slog.Info("control journal: writes recovered", "failing_since", r.failingAt, "dropped", r.dropped)
		r.failingAt, r.loggedAt, r.dropped = time.Time{}, time.Time{}, 0
	}
	for i, e := range batch {
		if e.orig.ID == 0 {
			e.orig.ID = copies[i].ID
		}
		if v, ok := r.pending[e.orig]; ok && v == e.version {
			delete(r.pending, e.orig)
		}
	}
	r.compactLocked()
}

// Open reports the spans that hold now, for tests and diagnostics.
func (r *Recorder) Open() []state.ControlSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]state.ControlSpan, 0, len(r.open))
	for _, sp := range r.open {
		out = append(out, *sp)
	}
	return out
}

func (r *Recorder) closeLocked(sp *state.ControlSpan, endMs int64) {
	sp.EndMs = endMs
	if endMs > sp.UpdatedMs {
		sp.UpdatedMs = endMs
	}
	r.markLocked(sp)
}

func (r *Recorder) markLocked(sp *state.ControlSpan) {
	r.version++
	if _, ok := r.pending[sp]; !ok {
		r.order = append(r.order, sp)
	}
	r.pending[sp] = r.version
	for len(r.pending) > maxPending {
		// The oldest closed span gives way; an open one is still needed.
		dropped := false
		for i, old := range r.order {
			if _, ok := r.pending[old]; ok && old.EndMs != 0 {
				delete(r.pending, old)
				r.order = append(r.order[:i], r.order[i+1:]...)
				r.dropped++
				dropped = true
				break
			}
		}
		if !dropped {
			break
		}
	}
}

// compactLocked drops written spans from the order list.
func (r *Recorder) compactLocked() {
	kept := r.order[:0]
	for _, sp := range r.order {
		if _, ok := r.pending[sp]; ok {
			kept = append(kept, sp)
		}
	}
	r.order = kept
}
